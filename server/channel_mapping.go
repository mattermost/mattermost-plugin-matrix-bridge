package main

import (
	"github.com/pkg/errors"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
)

// maxServersPerChannel is the number of Matrix servers a single channel may be bridged
// to. The stored value (kvstore.ChannelServerMapping) and every helper are already
// list-shaped, so raising this (or dropping the check entirely) to allow a channel to
// be mapped to several homeservers needs no migration - see §6 of the design doc.
const maxServersPerChannel = 1

// ErrChannelAlreadyMapped is the sentinel returned by SetChannelMapping; see
// kvstore.ErrChannelAlreadyMapped for why it lives in the kvstore package.
var ErrChannelAlreadyMapped = kvstore.ErrChannelAlreadyMapped

// ChannelMapper is the one choke point through which every channel<->room mapping
// write must go, so the maxServersPerChannel policy lives in exactly one place.
// Implemented by Plugin; BridgeUtils holds one so both command handlers
// (/matrix map, /matrix server map, /matrix create) and the bridge-internal paths
// (Matrix-initiated DM creation, outbound DM auto-creation) share the same CAS +
// policy logic instead of duplicating it.
type ChannelMapper interface {
	SetChannelMapping(channelID, serverID, roomID string) ([]kvstore.ChannelServerMapping, error)
}

// SetChannelMapping upserts serverID's entry for channelID via compare-and-set. The
// policy check runs INSIDE the CAS callback so two concurrent maps on one channel
// cannot both win the race and each believe they mapped the (only allowed) server.
//
// Semantics:
//   - Channel unmapped -> append the entry.
//   - Already mapped to this server -> overwrite the room. Callers are responsible for
//     cleaning up any now-stale room_mapping_ reverse keys for the old room (see
//     setChannelRoomMapping) - this function only owns the channel_mapping_ value.
//   - Already mapped to maxServersPerChannel OTHER live servers -> ErrChannelAlreadyMapped.
//   - Entries for servers no longer in the registry are stale and do not count toward
//     the limit; they are dropped in the same CAS write. Re-adopting that ServerID later
//     makes the entry live again with no re-mapping needed.
func (p *Plugin) SetChannelMapping(channelID, serverID, roomID string) ([]kvstore.ChannelServerMapping, error) {
	// Read the live registry once, before entering the CAS callback, and close over the
	// result: valueFunc may run more than once on retry and must stay a pure function of
	// the mapping slice it is given (no network/plugin-API calls inside it).
	liveServerIDs, err := p.liveServerIDSet()
	if err != nil {
		return nil, err
	}

	key := kvstore.BuildChannelMappingKey(channelID)
	var result []kvstore.ChannelServerMapping

	err = p.kvstore.SetAtomicWithRetries(key, func(oldValue []byte) ([]byte, error) {
		mappings, err := kvstore.ParseChannelServerMappings(oldValue)
		if err != nil {
			return nil, err
		}

		live := make([]kvstore.ChannelServerMapping, 0, len(mappings))
		mappedToThisServer := false
		otherLiveCount := 0
		for _, m := range mappings {
			if !liveServerIDs[m.ServerID] {
				continue // stale entry for a removed server - drop it
			}
			live = append(live, m)
			if m.ServerID == serverID {
				mappedToThisServer = true
			} else {
				otherLiveCount++
			}
		}

		if !mappedToThisServer && otherLiveCount >= maxServersPerChannel {
			return nil, ErrChannelAlreadyMapped
		}

		updated := kvstore.UpsertChannelServerMapping(live, serverID, roomID)
		result = updated
		return kvstore.MarshalChannelServerMappings(updated)
	})
	if err != nil {
		return nil, err
	}

	return result, nil
}

// liveServerIDSet returns the set of currently-registered server IDs, used to filter
// stale channel-mapping entries out of the maxServersPerChannel count.
func (p *Plugin) liveServerIDSet() (map[string]bool, error) {
	servers, err := p.getServers()
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(servers))
	for _, s := range servers {
		set[s.ServerID] = true
	}
	return set, nil
}

// MapChannelToServer maps channelID to matrixRoomIdentifier on serverID, going through
// this server's BridgeUtils so the resolve-alias + choke-point-write + reverse-key
// logic is shared with every other mapping path (inbound DM creation, outbound DM
// auto-creation). Used by command handlers (/matrix map, /matrix server map, /matrix
// create), which live in a different package and so cannot call BridgeUtils directly.
func (p *Plugin) MapChannelToServer(serverID, channelID, matrixRoomIdentifier string) error {
	utils, err := p.bridgeUtilsForServer(serverID)
	if err != nil {
		return err
	}
	return utils.setChannelRoomMapping(channelID, matrixRoomIdentifier)
}

// UnmapChannelFromServer removes serverID's mapping for channelID. Sequencing matters:
// the Matrix room state is cleared first (if that fails, the whole operation aborts,
// or sync messages would keep flowing to a channel Mattermost no longer considers
// mapped); then the channel_mapping_ entry is removed (deleting the key rather than
// storing an empty array, once no server remains mapped); then both room_mapping_
// reverse keys; then that server's shared-channels remote is uninvited.
//
// A corrupt (unparseable) mapping value is cleared with an explanatory error rather
// than reported as "not mapped".
func (p *Plugin) UnmapChannelFromServer(serverID, channelID string) error {
	client := p.getMatrixClient(serverID)
	if client == nil {
		return errors.Errorf("no Matrix client configured for server %s", serverID)
	}

	key := kvstore.BuildChannelMappingKey(channelID)
	data, err := p.kvstore.Get(key)
	if err != nil {
		return errors.Wrap(err, "failed to read channel mapping")
	}

	mappings, err := kvstore.ParseChannelServerMappings(data)
	if err != nil {
		if delErr := p.kvstore.Delete(key); delErr != nil {
			p.logger.LogWarn("Failed to delete corrupt channel mapping", "channel_id", channelID, "error", delErr)
		}
		return errors.Wrap(err, "channel mapping was corrupt and has been cleared; nothing to unmap")
	}

	roomID := kvstore.RoomIDForServer(mappings, serverID)
	if roomID == "" {
		return errors.Errorf("channel is not mapped to server %s", serverID)
	}

	if err := client.RemoveMattermostChannelID(roomID); err != nil {
		return errors.Wrap(err, "failed to clear Matrix room state; aborting unmap so sync does not continue")
	}

	if _, err := kvstore.RemoveServerFromChannelMapping(p.kvstore, key, serverID); err != nil {
		return errors.Wrap(err, "failed to remove channel mapping")
	}

	if err := p.kvstore.Delete(kvstore.BuildRoomMappingKey(serverID, roomID)); err != nil {
		p.logger.LogWarn("Failed to delete reverse room mapping", "server_id", serverID, "room_id", roomID, "error", err)
	}

	remoteID := p.remoteIDForServer(serverID)
	if remoteID != "" {
		if err := p.API.UninviteRemoteFromChannel(channelID, remoteID); err != nil {
			p.logger.LogWarn("Failed to uninvite plugin from shared channel", "channel_id", channelID, "remote_id", remoteID, "error", err)
		}
	}

	return nil
}
