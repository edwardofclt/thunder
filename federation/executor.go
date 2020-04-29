package federation

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/samsarahq/thunder/graphql"
	"github.com/samsarahq/thunder/graphql/introspection"
	"golang.org/x/sync/errgroup"
)

type ExecutorClient interface {
	Execute(ctx context.Context, req *graphql.Query) ([]byte, error)
}

// Executor has a map of all the executor clients such that it can execute a
// subquery on any of the federated servers.
// The planner allows it to coordinate the subqueries being sent to the federated servers
type Executor struct {
	Executors map[string]ExecutorClient
	planner   *Planner
}

func fetchSchema(ctx context.Context, e ExecutorClient) ([]byte, error) {
	query, err := graphql.Parse(introspection.IntrospectionQuery, map[string]interface{}{})
	if err != nil {
		return nil, err
	}

	return e.Execute(ctx, query)
}

func NewExecutor(ctx context.Context, executors map[string]ExecutorClient) (*Executor, error) {
	// Fetches the schemas from the executors clients
	schemas := make(map[string]*introspectionQueryResult)
	for server, client := range executors {
		schema, err := fetchSchema(ctx, client)
		if err != nil {
			return nil, fmt.Errorf("fetching schema %s: %v", server, err)
		}

		var iq introspectionQueryResult
		if err := json.Unmarshal(schema, &iq); err != nil {
			return nil, fmt.Errorf("unmarshaling schema %s: %v", server, err)
		}

		schemas[server] = &iq
	}

	types, err := convertSchema(schemas)
	if err != nil {
		return nil, fmt.Errorf("converting schema error: %v", err)
	}

	introspectionSchema := introspection.BareIntrospectionSchema(types.Schema)
	introspectionServer := &Server{schema: introspectionSchema}

	executors["introspection"] = &DirectExecutorClient{Client: introspectionServer}
	schema, err := introspection.RunIntrospectionQuery(introspection.BareIntrospectionSchema(introspectionServer.schema))

	var iq introspectionQueryResult
	if err := json.Unmarshal(schema, &iq); err != nil {
		return nil, fmt.Errorf("unmarshaling introspection schema: %v", err)
	}

	schemas["introspection"] = &iq
	types, err = convertSchema(schemas)
	if err != nil {
		return nil, err
	}

	flattener, err := newFlattener(types.Schema)
	if err != nil {
		return nil, err
	}

	// The planner is aware of the merged schema and what executors
	// know about what fields
	planner := &Planner{
		schema:    types,
		flattener: flattener,
	}

	return &Executor{
		Executors: executors,
		planner:   planner,
	}, nil

}

type pathFollower struct {
	targets []map[string]interface{}
	keys    []interface{}
}

func (pf *pathFollower) extractTargets(node interface{}, path []PathStep) error {
	if slice, ok := node.([]interface{}); ok {
		for i, elem := range slice {
			if err := pf.extractTargets(elem, path); err != nil {
				return fmt.Errorf("idx %d: %v", i, err)
			}
		}
		return nil
	}

	if len(path) == 0 {
		obj, ok := node.(map[string]interface{})
		if !ok {
			return fmt.Errorf("not an object: %v", obj)
		}
		key, ok := obj["__federation"]
		if !ok {
			return fmt.Errorf("missing __federation: %v", obj)
		}
		pf.targets = append(pf.targets, obj)
		pf.keys = append(pf.keys, key)
		return nil
	}

	obj, ok := node.(map[string]interface{})
	if !ok {
		return nil
	}

	step := path[0]
	switch step.Kind {
	case KindField:
		next, ok := obj[step.Name]
		if !ok {
			return fmt.Errorf("does not have key %s", step.Name)
		}

		if err := pf.extractTargets(next, path[1:]); err != nil {
			return fmt.Errorf("elem %s: %v", next, err)
		}

	case KindType:
		typ, ok := obj["__typename"].(string)
		if !ok {
			return fmt.Errorf("does not have string key __typename")
		}

		if typ == step.Name {
			if err := pf.extractTargets(obj, path[1:]); err != nil {
				return fmt.Errorf("typ %s: %v", typ, err)
			}
		}
	}

	return nil
}

func (e *Executor) execute(ctx context.Context, p *Plan, keys []interface{}) ([]interface{}, error) {
	var res []interface{}

	if p.Service == gatewayCoordinatorServiceName {
		res = []interface{}{
			map[string]interface{}{},
		}
	}

	g, ctx := errgroup.WithContext(ctx)

	var resMu sync.Mutex

	for _, subPlan := range p.After {
		subPlan := subPlan
		var pf pathFollower
		pf.keys = nil
		pf.targets = []map[string]interface{}{
			res[0].(map[string]interface{}),
		}

		g.Go(func() error {
			results, err := e.execute(ctx, subPlan, pf.keys)
			if err != nil {
				return fmt.Errorf("executing sub plan: %v", err)
			}

			if len(results) != len(pf.targets) {
				return fmt.Errorf("got %d results for %d targets", len(results), len(pf.targets))
			}

			resMu.Lock()
			defer resMu.Unlock()

			for i, target := range pf.targets {
				result, ok := results[i].(map[string]interface{})
				if !ok {
					return fmt.Errorf("result is not an object: %v", result)
				}
				for k, v := range result {
					target[k] = v
				}
			}

			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}
	return res, nil
}

func (e *Executor) Execute(ctx context.Context, q *graphql.Query) (interface{}, error) {
	p, err := e.planner.planRoot(q)
	if err != nil {
		return nil, err
	}

	printPlan(p)
	r, err := e.execute(ctx, p, nil)
	if err != nil {
		return nil, err
	}
	res := r[0]

	return res, nil

}