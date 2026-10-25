package gateway

import (
	"context"

	"github.com/keakon/chord-gateway/internal/im"
)

func (s *IngressStore) routePending(id im.ChatIdentity) []IngressRecord {
	var out []IngressRecord
	for key := range s.pendingByRoute[id] {
		out = append(out, s.records[key])
	}
	return out
}
func (s *IngressStore) EnsureRoute(ctx context.Context, id im.ChatIdentity, digest, workspace, session string) (IngressRoute, error) {
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
		if r.State == im.IngressDispatching || r.State == im.IngressUnknown {
			return IngressRoute{}, errIngressBlocked
		}
	}
	for _, r := range records {
		if r.State == im.IngressReceived {
			r.State = im.IngressQuarantined
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
func (s *IngressStore) BeginRouteChange(ctx context.Context, id im.ChatIdentity) (IngressRoute, error) {
	if err := s.lock(ctx); err != nil {
		return IngressRoute{}, err
	}
	defer s.mu.Unlock()
	for _, r := range s.routePending(id) {
		// A queued task owns the route; a queued control can be re-targeted or
		// dropped by the change itself. Anything already in flight blocks
		// regardless of kind.
		if r.State == im.IngressDispatching || r.State == im.IngressUnknown || (r.Kind == "task" && r.State == im.IngressReceived) {
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
func (s *IngressStore) FinishRouteChange(ctx context.Context, id im.ChatIdentity, revision, digest, workspace, session string) error {
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
func (s *IngressStore) BindRouteSession(ctx context.Context, id im.ChatIdentity, revision, session string) error {
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
		if r.State == im.IngressReceived && r.RouteRevision == revision && r.SessionID == "" {
			r.SessionID = session
			r.UpdatedAt = s.now()
			s.save(r)
		}
	}
	route.SessionID = session
	s.routes[id] = route
	return nil
}
