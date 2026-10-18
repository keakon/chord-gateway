package main

import "context"

func (s *IngressStore) routePending(id ChatIdentity) []IngressRecord {
	var out []IngressRecord
	for key := range s.pendingByRoute[id] {
		out = append(out, s.records[key])
	}
	return out
}
func (s *IngressStore) EnsureRoute(ctx context.Context, id ChatIdentity, digest, workspace, session string) (IngressRoute, error) {
	if err := s.lock(ctx); err != nil {
		return IngressRoute{}, err
	}
	defer s.mu.Unlock()
	s.pruneHistory(id)
	route := s.routes[id]
	if route.Phase == "active" && route.AuthorizationDigest == digest && route.WorkspaceID == workspace && route.SessionID == session {
		return route, nil
	}
	records := s.routePending(id)
	for _, r := range records {
		if r.State == ingressDispatching || r.State == ingressUnknown {
			return IngressRoute{}, errIngressBlocked
		}
	}
	for _, r := range records {
		if r.State == ingressReceived {
			r.State = ingressQuarantined
			r.Reason = "route_identity_changed"
			r.UpdatedAt = s.now()
			s.save(r)
		}
	}
	route.Identity = id
	route.Revision = newIngressID()
	route.AuthorizationDigest = digest
	route.WorkspaceID = workspace
	route.SessionID = session
	route.Phase = "active"
	s.routes[id] = route
	return route, nil
}
func (s *IngressStore) BeginRouteChange(ctx context.Context, id ChatIdentity) (IngressRoute, error) {
	if err := s.lock(ctx); err != nil {
		return IngressRoute{}, err
	}
	defer s.mu.Unlock()
	for _, r := range s.routePending(id) {
		// A queued task owns the route; a queued control can be re-targeted or
		// dropped by the change itself. Anything already in flight blocks
		// regardless of kind.
		if r.State == ingressDispatching || r.State == ingressUnknown || (r.Kind == "task" && r.State == ingressReceived) {
			return IngressRoute{}, errIngressBlocked
		}
	}
	route := s.routes[id]
	route.Identity = id
	route.Revision = newIngressID()
	route.Phase = "changing"
	s.routes[id] = route
	return route, nil
}
func (s *IngressStore) FinishRouteChange(ctx context.Context, id ChatIdentity, revision, digest, workspace, session string) error {
	if err := s.lock(ctx); err != nil {
		return err
	}
	defer s.mu.Unlock()
	route, ok := s.routes[id]
	if !ok {
		return errIngressMissing
	}
	if route.Revision != revision || route.Phase != "changing" {
		return errIngressBlocked
	}
	route.AuthorizationDigest = digest
	route.WorkspaceID = workspace
	route.SessionID = session
	route.Phase = "active"
	s.routes[id] = route
	return nil
}
func (s *IngressStore) BindRouteSession(ctx context.Context, id ChatIdentity, revision, session string) error {
	if err := s.lock(ctx); err != nil {
		return err
	}
	defer s.mu.Unlock()
	route, ok := s.routes[id]
	if !ok {
		return errIngressMissing
	}
	if session == "" || route.Phase != "active" || route.Revision != revision || route.SessionID != "" && route.SessionID != session {
		return errIngressBlocked
	}
	for _, r := range s.routePending(id) {
		if r.State == ingressReceived && r.RouteRevision == revision && r.SessionID == "" {
			r.SessionID = session
			r.UpdatedAt = s.now()
			s.save(r)
		}
	}
	route.SessionID = session
	s.routes[id] = route
	return nil
}
