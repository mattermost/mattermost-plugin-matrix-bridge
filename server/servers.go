package main

// serverDomainForID returns the ServerName (Matrix ID domain) for a registered server,
// resolved through serverConfigForRouting rather than servers.Service.Domain: isGhostUser
// calls this once per inbound Matrix event, and Service.Domain would unmarshal the whole
// registry out of KV every time. The snapshot is refreshed by every registry mutation, so
// this is never staler than a direct registry read would have been.
func (p *Plugin) serverDomainForID(serverID string) (string, error) {
	server, err := p.serverConfigForRouting(serverID)
	if err != nil {
		return "", err
	}
	return server.ServerName, nil
}

// registeredServerIDForRemote resolves a shared-channels remote ID against the registry
// itself rather than this node's remoteToServerID cache, returning "" when no registered
// server claims it. This is the authoritative answer to "is this remote one of ours?",
// and it costs a single KV read - no client construction, no rebuild mutex - so a cache
// miss on a hot path can be disambiguated without paying for initMatrixClients.
func (p *Plugin) registeredServerIDForRemote(remoteID string) (string, error) {
	if remoteID == "" {
		return "", nil
	}
	list, err := p.servers.List()
	if err != nil {
		return "", err
	}
	for _, s := range list {
		if s.RemoteID == remoteID {
			return s.ServerID, nil
		}
	}
	return "", nil
}
