// MCP workspace-root resolution (#120).
//
// Over stdio the server cannot see the client's shell cwd, so an omitted
// project_root historically fell back to the *server's* launch dir — which,
// for one server shared across worktrees, is the main checkout. The MCP `roots`
// capability is the protocol-native fix: the client declares its workspace and
// the server asks for it via roots/list. This file carries the client session
// (stashed into the request context by addTool) and the roots lookup that
// resolveProject consults before the cwd backstop.

package mcp

import (
	"context"
	"net/url"
	"path/filepath"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/alehatsman/dex/internal/proj"
)

// rootsSessionKey is the context key under which addTool stashes the live MCP
// session, so resolveProject can reach the client's roots without threading the
// request through 34 handler signatures. A distinct type satisfies revive's
// context-keys-type rule.
type rootsSessionKey struct{}

// rootLister is the slice of *sdk.ServerSession that root resolution needs:
// the server→client roots/list call. Tests inject a fake; production passes the
// live session.
type rootLister interface {
	ListRoots(context.Context, *sdk.ListRootsParams) (*sdk.ListRootsResult, error)
}

// withSession returns ctx carrying the client session as a rootLister, or ctx
// unchanged when ss is nil (the CLI path, which calls handlers with a nil
// request).
func withSession(ctx context.Context, ss *sdk.ServerSession) context.Context {
	if ss == nil {
		return ctx // nil-check the concrete pointer before it becomes a non-nil interface
	}
	return withLister(ctx, ss)
}

// withLister stashes a rootLister directly. Production goes through withSession
// (which nil-guards the concrete session); tests inject a fake lister here
// without fabricating an SDK session.
func withLister(ctx context.Context, l rootLister) context.Context {
	return context.WithValue(ctx, rootsSessionKey{}, l)
}

// listerFromContext recovers the session stashed by withSession, or nil when
// none is present (CLI path, or a client without an active session).
func listerFromContext(ctx context.Context) rootLister {
	l, _ := ctx.Value(rootsSessionKey{}).(rootLister)
	return l
}

// rootsProbeTimeout bounds the roots/list round-trip so a slow or unresponsive
// client cannot hang a tool call.
const rootsProbeTimeout = 2 * time.Second

// rootFromClient asks the client for its workspace roots and returns the first
// file:// root that resolves to an existing directory, or "" on any error,
// empty list, unsupported capability, or timeout. Never fatal — the caller
// falls through to the cwd backstop.
func rootFromClient(ctx context.Context, l rootLister, base string) string {
	cctx, cancel := context.WithTimeout(ctx, rootsProbeTimeout)
	defer cancel()
	res, err := l.ListRoots(cctx, nil)
	if err != nil || res == nil {
		return ""
	}
	for _, r := range res.Roots {
		if r == nil {
			continue
		}
		p := fileURIToPath(r.URI)
		if p == "" {
			continue
		}
		// proj.Resolve canonicalizes and stats the path; a non-existent or
		// non-directory root fails here and we try the next one.
		if _, err := proj.Resolve(p, base); err == nil {
			return p
		}
	}
	return ""
}

// fileURIToPath converts a file:// root URI to a local path, or "" for a
// non-file or unparseable URI. A bare absolute path (no scheme) is accepted
// as-is, for lenient clients that send a plain path.
func fileURIToPath(uri string) string {
	if uri == "" {
		return ""
	}
	if filepath.IsAbs(uri) {
		return uri
	}
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		return ""
	}
	return u.Path
}
