package evaluator

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/open-policy-agent/opa/rego"

	"github.com/pomerium/pomerium/internal/telemetry/trace"
)

// A CustomEvaluatorRequest is the data needed to evaluate a custom rego policy.
type CustomEvaluatorRequest struct {
	RegoPolicy string
	HTTP       RequestHTTP    `json:"http"`
	Session    RequestSession `json:"session"`
}

// A CustomEvaluatorResponse is the response from the evaluation of a custom rego policy.
type CustomEvaluatorResponse struct {
	Allowed bool
	Denied  bool
	Reason  string
	Headers map[string]string
}

// A CustomEvaluator evaluates custom rego policies.
type CustomEvaluator struct {
	store   *Store
	mu      sync.Mutex
	queries map[string]rego.PreparedEvalQuery
}

// NewCustomEvaluator creates a new CustomEvaluator.
func NewCustomEvaluator(store *Store) *CustomEvaluator {
	ce := &CustomEvaluator{
		store:   store,
		queries: map[string]rego.PreparedEvalQuery{},
	}
	return ce
}

// Evaluate evaluates the custom rego policy.
func (ce *CustomEvaluator) Evaluate(ctx context.Context, req *CustomEvaluatorRequest) (*CustomEvaluatorResponse, error) {
	_, span := trace.StartSpan(ctx, "authorize.evaluator.custom.Evaluate")
	defer span.End()

	q, err := ce.getPreparedEvalQuery(ctx, req.RegoPolicy)
	if err != nil {
		return nil, err
	}

	resultSet, err := safeEval(ctx, q, rego.EvalInput(struct {
		HTTP    RequestHTTP    `json:"http"`
		Session RequestSession `json:"session"`
	}{HTTP: req.HTTP, Session: req.Session}))
	if err != nil {
		return nil, err
	}

	vars := ce.getVars(resultSet)
	res := &CustomEvaluatorResponse{
		Headers: getHeadersVar(vars),
	}
	if result, ok := vars["result"].(map[string]interface{}); ok {
		res.Allowed, _ = result["allow"].(bool)
		if v, ok := result["deny"]; ok {
			// support `deny = true`
			if b, ok := v.(bool); ok {
				res.Denied = b
			}

			// support `deny[reason] = true`
			if m, ok := v.(map[string]interface{}); ok {
				for mk, mv := range m {
					if b, ok := mv.(bool); ok {
						res.Denied = b
						res.Reason = mk
					}
				}
			}
		}
	}
	return res, nil
}

func (ce *CustomEvaluator) getPreparedEvalQuery(ctx context.Context, src string) (rego.PreparedEvalQuery, error) {
	ce.mu.Lock()
	defer ce.mu.Unlock()

	q, ok := ce.queries[src]
	if ok {
		return q, nil
	}

	r := rego.New(
		rego.Store(ce.store),
		rego.Module("pomerium.custom_policy", src),
		rego.Query("result = data.pomerium.custom_policy"),
		getGoogleCloudServerlessHeadersRegoOption,
		ce.store.GetDataBrokerRecordOption(),
	)
	q, err := r.PrepareForEval(ctx)
	if err != nil {
		// if no package is in the src, add it
		if strings.Contains(err.Error(), "package expected") {
			r = rego.New(
				rego.Store(ce.store),
				rego.Module("pomerium.custom_policy", "package pomerium.custom_policy\n\n"+src),
				rego.Query("result = data.pomerium.custom_policy"),
				getGoogleCloudServerlessHeadersRegoOption,
				ce.store.GetDataBrokerRecordOption(),
			)
			q, err = r.PrepareForEval(ctx)
		}
	}
	if err != nil {
		return q, fmt.Errorf("invalid rego policy: %w", err)
	}

	ce.queries[src] = q
	return q, nil
}

func (ce *CustomEvaluator) getVars(resultSet rego.ResultSet) rego.Vars {
	if len(resultSet) == 0 {
		return make(rego.Vars)
	}
	return resultSet[0].Bindings.WithoutWildcards()
}
