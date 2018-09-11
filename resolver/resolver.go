// Package resolver implements utilities for resolving paths within dms3fs.
package resolver

import (
	"context"
	"errors"
	"fmt"
	"time"

	path "github.com/dms3-fs/go-path"

	cid "github.com/dms3-fs/go-cid"
	dms3ld "github.com/dms3-fs/go-ld-format"
	logging "github.com/dms3-fs/go-log"
	dag "github.com/dms3-fs/go-merkledag"
)

var log = logging.Logger("pathresolv")

// ErrNoComponents is used when Paths after a protocol
// do not contain at least one component
var ErrNoComponents = errors.New(
	"path must contain at least one component")

// ErrNoLink is returned when a link is not found in a path
type ErrNoLink struct {
	Name string
	Node *cid.Cid
}

// Error implements the Error interface for ErrNoLink with a useful
// human readable message.
func (e ErrNoLink) Error() string {
	return fmt.Sprintf("no link named %q under %s", e.Name, e.Node.String())
}

// ResolveOnce resolves path through a single node
type ResolveOnce func(ctx context.Context, ds dms3ld.NodeGetter, nd dms3ld.Node, names []string) (*dms3ld.Link, []string, error)

// Resolver provides path resolution to DMS3FS
// It has a pointer to a DAGService, which is uses to resolve nodes.
// TODO: now that this is more modular, try to unify this code with the
//       the resolvers in namesys
type Resolver struct {
	DAG dms3ld.NodeGetter

	ResolveOnce ResolveOnce
}

// NewBasicResolver constructs a new basic resolver.
func NewBasicResolver(ds dms3ld.DAGService) *Resolver {
	return &Resolver{
		DAG:         ds,
		ResolveOnce: ResolveSingle,
	}
}

// ResolveToLastNode walks the given path and returns the cid of the last node
// referenced by the path
func (r *Resolver) ResolveToLastNode(ctx context.Context, fpath path.Path) (*cid.Cid, []string, error) {
	c, p, err := path.SplitAbsPath(fpath)
	if err != nil {
		return nil, nil, err
	}

	if len(p) == 0 {
		return c, nil, nil
	}

	nd, err := r.DAG.Get(ctx, c)
	if err != nil {
		return nil, nil, err
	}

	for len(p) > 0 {
		lnk, rest, err := r.ResolveOnce(ctx, r.DAG, nd, p)

		// Note: have to drop the error here as `ResolveOnce` doesn't handle 'leaf'
		// paths (so e.g. for `echo '{"foo":123}' | dms3fs dag put` we wouldn't be
		// able to resolve `zdpu[...]/foo`)
		if lnk == nil {
			break
		}

		if err != nil {
			return nil, nil, err
		}

		next, err := lnk.GetNode(ctx, r.DAG)
		if err != nil {
			return nil, nil, err
		}
		nd = next
		p = rest
	}

	if len(p) == 0 {
		return nd.Cid(), nil, nil
	}

	// Confirm the path exists within the object
	val, rest, err := nd.Resolve(p)
	if err != nil {
		return nil, nil, err
	}

	if len(rest) > 0 {
		return nil, nil, errors.New("path failed to resolve fully")
	}
	switch val.(type) {
	case *dms3ld.Link:
		return nil, nil, errors.New("inconsistent ResolveOnce / nd.Resolve")
	default:
		return nd.Cid(), p, nil
	}
}

// ResolvePath fetches the node for given path. It returns the last item
// returned by ResolvePathComponents.
func (r *Resolver) ResolvePath(ctx context.Context, fpath path.Path) (dms3ld.Node, error) {
	// validate path
	if err := fpath.IsValid(); err != nil {
		return nil, err
	}

	nodes, err := r.ResolvePathComponents(ctx, fpath)
	if err != nil || nodes == nil {
		return nil, err
	}
	return nodes[len(nodes)-1], err
}

// ResolveSingle simply resolves one hop of a path through a graph with no
// extra context (does not opaquely resolve through sharded nodes)
func ResolveSingle(ctx context.Context, ds dms3ld.NodeGetter, nd dms3ld.Node, names []string) (*dms3ld.Link, []string, error) {
	return nd.ResolveLink(names)
}

// ResolvePathComponents fetches the nodes for each segment of the given path.
// It uses the first path component as a hash (key) of the first node, then
// resolves all other components walking the links, with ResolveLinks.
func (r *Resolver) ResolvePathComponents(ctx context.Context, fpath path.Path) ([]dms3ld.Node, error) {
	evt := log.EventBegin(ctx, "resolvePathComponents", logging.LoggableMap{"fpath": fpath})
	defer evt.Done()

	h, parts, err := path.SplitAbsPath(fpath)
	if err != nil {
		evt.Append(logging.LoggableMap{"error": err.Error()})
		return nil, err
	}

	log.Debug("resolve dag get")
	nd, err := r.DAG.Get(ctx, h)
	if err != nil {
		evt.Append(logging.LoggableMap{"error": err.Error()})
		return nil, err
	}

	return r.ResolveLinks(ctx, nd, parts)
}

// ResolveLinks iteratively resolves names by walking the link hierarchy.
// Every node is fetched from the DAGService, resolving the next name.
// Returns the list of nodes forming the path, starting with ndd. This list is
// guaranteed never to be empty.
//
// ResolveLinks(nd, []string{"foo", "bar", "baz"})
// would retrieve "baz" in ("bar" in ("foo" in nd.Links).Links).Links
func (r *Resolver) ResolveLinks(ctx context.Context, ndd dms3ld.Node, names []string) ([]dms3ld.Node, error) {

	evt := log.EventBegin(ctx, "resolveLinks", logging.LoggableMap{"names": names})
	defer evt.Done()
	result := make([]dms3ld.Node, 0, len(names)+1)
	result = append(result, ndd)
	nd := ndd // dup arg workaround

	// for each of the path components
	for len(names) > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Minute)
		defer cancel()

		lnk, rest, err := r.ResolveOnce(ctx, r.DAG, nd, names)
		if err == dag.ErrLinkNotFound {
			evt.Append(logging.LoggableMap{"error": err.Error()})
			return result, ErrNoLink{Name: names[0], Node: nd.Cid()}
		} else if err != nil {
			evt.Append(logging.LoggableMap{"error": err.Error()})
			return result, err
		}

		nextnode, err := lnk.GetNode(ctx, r.DAG)
		if err != nil {
			evt.Append(logging.LoggableMap{"error": err.Error()})
			return result, err
		}

		nd = nextnode
		result = append(result, nextnode)
		names = rest
	}
	return result, nil
}
