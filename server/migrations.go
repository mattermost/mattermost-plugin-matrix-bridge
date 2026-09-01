package main

import (
	"strconv"

	"github.com/pkg/errors"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
)

// runKVStoreMigrations brings the KV store up to kvstore.CurrentKVStoreVersion. Version 1
// (the multi-server layout) has no upgrade path: the single step deletes every
// pre-multi-server record rather than translating it, so an upgrading install must
// re-register its homeservers and re-map its channels.
func (p *Plugin) runKVStoreMigrations() error {
	currentVersion := 0
	versionBytes, err := p.kvstore.Get(kvstore.KeySchemaVersion)
	if err != nil {
		return errors.Wrap(err, "failed to read KV store schema version")
	}
	if len(versionBytes) > 0 {
		parsed, parseErr := strconv.Atoi(string(versionBytes))
		if parseErr != nil {
			return errors.Wrapf(parseErr, "KV store schema version %q is not a number; refusing to guess whether records need purging", string(versionBytes))
		}
		currentVersion = parsed
	}

	if currentVersion >= kvstore.CurrentKVStoreVersion {
		p.logger.LogDebug("KV store is up to date", "version", currentVersion)
		return nil
	}

	p.logger.LogInfo("Running KV store migrations", "from_version", currentVersion, "target_version", kvstore.CurrentKVStoreVersion)

	if err := p.purgeStaleRecords(); err != nil {
		return errors.Wrap(err, "failed to purge pre-multi-server KV records")
	}

	// Stamped only after a fully succeeded purge: a marker claiming the store is current
	// while stale records survive is the one state no later run would correct.
	if err := p.kvstore.Set(kvstore.KeySchemaVersion, []byte(strconv.Itoa(kvstore.CurrentKVStoreVersion))); err != nil {
		return errors.Wrap(err, "failed to update KV store schema version")
	}

	p.logger.LogInfo("KV store migrations completed successfully", "new_version", kvstore.CurrentKVStoreVersion)
	return nil
}

// purgeStaleRecords deletes every record under kvstore.BridgeDataPrefixes, plus the
// pre-multi-server version marker. Safe to run unconditionally because it only ever runs
// before the marker exists, which is before OnActivate registers the slash command, so no
// server can have been added and no channel mapped yet.
func (p *Plugin) purgeStaleRecords() error {
	keysByPrefix, err := kvstore.ListAllKeysByPrefix(p.kvstore, kvstore.DefaultListKeysBatchSize, kvstore.BridgeDataPrefixes...)
	if err != nil {
		return errors.Wrap(err, "failed to enumerate bridge KV prefixes")
	}

	deleted := 0
	for _, prefix := range kvstore.BridgeDataPrefixes {
		for _, key := range keysByPrefix[prefix] {
			if err := p.kvstore.Delete(key); err != nil {
				return errors.Wrapf(err, "failed to delete stale record %q", key)
			}
			deleted++
		}
	}

	if err := p.kvstore.Delete(kvstore.KeyLegacyStoreVersion); err != nil {
		return errors.Wrap(err, "failed to delete the pre-multi-server version marker")
	}

	if deleted > 0 {
		p.logger.LogWarn("Deleted pre-multi-server Matrix bridge KV records; homeservers must be registered with `/matrix server add` and channels re-mapped",
			"deleted_record_count", deleted)
	}

	return nil
}
