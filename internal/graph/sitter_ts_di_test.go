package graph

import (
	"context"
	"testing"
)

// TestTSExtractorDIDispatch exercises name-based DI/adapter dispatch (#85):
// `this.field.method()` resolves through the field's known type to a concrete
// class method, where the field type comes from a constructor
// parameter-property, a typed field declaration, or a `field = new T()`
// initializer, and the type is either same-file or imported by name.
func TestTSExtractorDIDispatch(t *testing.T) {
	root := copyFixture(t, "ts_di")
	reg := NewRegistry()
	reg.Register(newTSTagsExtractor)

	res, err := ExtractSitterWith(context.Background(), root, reg)
	if err != nil {
		t.Fatalf("ExtractSitterWith: %v", err)
	}

	usPkg := "src/user_service"
	doLoginID := NodeID("", usPkg, NodeMethod, "UserService.doLogin")
	if findByID(res.Nodes, doLoginID) == nil {
		t.Fatalf("missing method UserService.doLogin; methods=%v", nodesOfKindWithPkg(res.Nodes, NodeMethod))
	}

	authLoginID := NodeID("", "src/auth", NodeMethod, "AuthService.login")
	loggerInfoID := NodeID("", "src/logger", NodeMethod, "Logger.info")
	cacheGetID := NodeID("", "src/cache", NodeMethod, "Cache.get")
	sessionEndID := NodeID("", usPkg, NodeMethod, "Session.end")

	calls := []struct {
		name string
		dst  string
		why  string
	}{
		{"this.auth.login (constructor param-property, imported)", authLoginID,
			"private readonly auth: AuthService → AuthService.login"},
		{"this.log.info (constructor param-property, imported)", loggerInfoID,
			"public log: Logger → Logger.info"},
		{"this.cache.get (field = new T(), imported)", cacheGetID,
			"private cache = new Cache() → Cache.get"},
		{"this.session.end (typed field, same-file)", sessionEndID,
			"protected session: Session → Session.end (local class)"},
	}
	for _, c := range calls {
		t.Run(c.name, func(t *testing.T) {
			if findEdge(res.Edges, EdgeCalls, doLoginID, c.dst) == nil {
				t.Errorf("missing DI calls edge doLogin → %s\n  why: %s\n  all calls=%v",
					c.name, c.why, edgeKinds(res.Edges, EdgeCalls))
			}
		})
	}
}
