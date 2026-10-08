/*
Copyright The Platform Mesh Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	appcontext "go.platform-mesh.io/search-service/internal/context"
	"go.platform-mesh.io/search-service/internal/httperr"
	"go.platform-mesh.io/search-service/internal/service/search"
)

type fakeSearchService struct {
	response search.SearchResponse
	err      error
	lastReq  search.SearchRequest
	reqs     []search.SearchRequest

	resourcesResp search.SearchResourcesResponse
	resourcesErr  error
	lastResReq    search.SearchResourcesRequest

	filterValuesResp search.FilterValuesResponse
	filterValuesErr  error
	lastFilterReq    search.FilterValuesRequest
}

func (f *fakeSearchService) Search(ctx context.Context, req search.SearchRequest) (search.SearchResponse, error) {
	f.lastReq = req
	f.reqs = append(f.reqs, req)
	return f.response, f.err
}

func (f *fakeSearchService) ListResources(ctx context.Context, req search.SearchResourcesRequest) (search.SearchResourcesResponse, error) {
	f.lastResReq = req
	return f.resourcesResp, f.resourcesErr
}

func (f *fakeSearchService) FilterValues(ctx context.Context, req search.FilterValuesRequest) (search.FilterValuesResponse, error) {
	f.lastFilterReq = req
	return f.filterValuesResp, f.filterValuesErr
}

func withRequestContext(rc appcontext.RequestContext) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(appcontext.WithRequestContext(r.Context(), rc)))
		})
	}
}

func TestCreateRouterSearchSuccess(t *testing.T) {
	svc := &fakeSearchService{response: search.SearchResponse{Results: []search.SearchHit{{ID: "1", Score: 1, Source: map[string]any{"id": "1"}}}}}
	r := CreateRouter(svc, []func(http.Handler) http.Handler{withRequestContext(appcontext.RequestContext{Organization: "acme", User: "alice@example.com"})})

	req := httptest.NewRequest(
		http.MethodGet,
		"/rest/v1/search?q=hello&mode=semantic&limit=15&page=3&cursor=abc&resource=accounts&filter.status=Ready&filter.fga_role=owner",
		nil,
	)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rr.Code)
	}
	if svc.lastReq.Organization != "acme" || svc.lastReq.User != "alice@example.com" {
		t.Fatalf("unexpected request context: %+v", svc.lastReq)
	}
	requestFieldsMatch := svc.lastReq.Query == "hello" &&
		svc.lastReq.Mode == search.SearchModeSemantic &&
		svc.lastReq.Limit == 15 &&
		svc.lastReq.Page == 3 &&
		svc.lastReq.Cursor == "abc" &&
		svc.lastReq.Resource == "accounts"
	if !requestFieldsMatch {
		t.Fatalf("unexpected request payload: %+v", svc.lastReq)
	}
	if len(svc.lastReq.Filters["status"]) != 1 || svc.lastReq.Filters["status"][0] != "Ready" {
		t.Fatalf("unexpected filters: %+v", svc.lastReq.Filters)
	}
	if svc.lastReq.FGARole != "owner" {
		t.Fatalf("unexpected FGA role: %q", svc.lastReq.FGARole)
	}
	if _, ok := svc.lastReq.Filters["fga_role"]; ok {
		t.Fatalf("FGA role must not be forwarded as a document filter: %+v", svc.lastReq.Filters)
	}

	var payload search.SearchResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not valid json: %v", err)
	}
	if len(payload.Results) != 1 {
		t.Fatalf("expected one result")
	}
}

func TestCreateRouterSearchAcceptsFirstPage(t *testing.T) {
	svc := &fakeSearchService{}
	r := CreateRouter(svc, []func(http.Handler) http.Handler{
		withRequestContext(appcontext.RequestContext{Organization: "acme", User: "alice@example.com"}),
	})
	req := httptest.NewRequest(http.MethodGet, "/rest/v1/search?q=hello&page=1", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rr.Code)
	}
	if svc.lastReq.Page != 1 {
		t.Fatalf("expected page 1, got %d", svc.lastReq.Page)
	}
}

func TestCreateRouterSearchResponseContract(t *testing.T) {
	next := "opaque-cursor"
	svc := &fakeSearchService{
		response: search.SearchResponse{
			Results: []search.SearchHit{{
				ID:     "res-1",
				Score:  12.34,
				Kind:   "Component",
				Name:   "my-component",
				Source: map[string]any{"id": "res-1", "kind": "Component"},
			}},
			NextCursor: &next,
		},
	}
	r := CreateRouter(svc, []func(http.Handler) http.Handler{withRequestContext(appcontext.RequestContext{Organization: "acme", User: "alice@example.com"})})
	req := httptest.NewRequest(http.MethodGet, "/rest/v1/search?q=hello", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not valid json: %v", err)
	}

	if _, ok := payload["results"]; !ok {
		t.Fatalf("missing results field")
	}
	if _, ok := payload["nextCursor"]; !ok {
		t.Fatalf("missing nextCursor field")
	}
	if _, ok := payload["totalCount"]; ok {
		t.Fatalf("totalCount should be omitted for cursor pagination")
	}

	results, ok := payload["results"].([]any)
	if !ok || len(results) != 1 {
		t.Fatalf("expected results array with one element")
	}
	first, ok := results[0].(map[string]any)
	if !ok {
		t.Fatalf("expected object result")
	}
	if _, ok := first["id"]; !ok {
		t.Fatalf("missing result id field")
	}
	if _, ok := first["score"]; !ok {
		t.Fatalf("missing result score field")
	}
	if _, ok := first["source"]; !ok {
		t.Fatalf("missing result source field")
	}
}

func TestCreateRouterSearchPageResponseContract(t *testing.T) {
	totalCount := 0
	svc := &fakeSearchService{
		response: search.SearchResponse{
			Results:    []search.SearchHit{},
			TotalCount: &totalCount,
		},
	}
	r := CreateRouter(svc, []func(http.Handler) http.Handler{
		withRequestContext(appcontext.RequestContext{Organization: "acme", User: "alice@example.com"}),
	})
	req := httptest.NewRequest(http.MethodGet, "/rest/v1/search?q=hello&page=2", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not valid json: %v", err)
	}
	if got := payload["totalCount"]; got != float64(totalCount) {
		t.Fatalf("expected totalCount %d, got %v", totalCount, got)
	}
	if _, ok := payload["nextCursor"]; ok {
		t.Fatalf("nextCursor should be omitted for page pagination")
	}
}

func TestCreateRouterMissingContextUnauthorized(t *testing.T) {
	svc := &fakeSearchService{}
	r := CreateRouter(svc, nil)
	req := httptest.NewRequest(http.MethodGet, "/rest/v1/search?q=hello", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

func TestCreateRouterInvalidLimit(t *testing.T) {
	svc := &fakeSearchService{}
	r := CreateRouter(svc, []func(http.Handler) http.Handler{withRequestContext(appcontext.RequestContext{Organization: "acme", User: "alice@example.com"})})
	req := httptest.NewRequest(http.MethodGet, "/rest/v1/search?q=hello&limit=bad", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

func TestCreateRouterInvalidPage(t *testing.T) {
	tests := []struct {
		name string
		page string
	}{
		{name: "not a number", page: "bad"},
		{name: "zero", page: "0"},
		{name: "negative", page: "-1"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := &fakeSearchService{}
			r := CreateRouter(svc, []func(http.Handler) http.Handler{
				withRequestContext(appcontext.RequestContext{Organization: "acme", User: "alice@example.com"}),
			})
			req := httptest.NewRequest(http.MethodGet, "/rest/v1/search?q=hello&page="+tc.page, nil)
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, req)

			if rr.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d", rr.Code)
			}
		})
	}
}

func TestCreateRouterSearchRejectsFGARoleAcrossAllResources(t *testing.T) {
	svc := &fakeSearchService{}
	r := CreateRouter(svc, []func(http.Handler) http.Handler{
		withRequestContext(appcontext.RequestContext{Organization: "acme", User: "alice@example.com"}),
	})
	req := httptest.NewRequest(http.MethodGet, "/rest/v1/search?q=hello&filter.fga_role=owner", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	if len(svc.reqs) != 0 {
		t.Fatalf("service must not be called for an all-resource role filter: %+v", svc.reqs)
	}
}

func TestCreateRouterSearchAcceptsFGARoleForExplicitResources(t *testing.T) {
	svc := &fakeSearchService{}
	r := CreateRouter(svc, []func(http.Handler) http.Handler{
		withRequestContext(appcontext.RequestContext{Organization: "acme", User: "alice@example.com"}),
	})
	req := httptest.NewRequest(http.MethodGet, "/rest/v1/search?q=hello&resources=accounts,components&filter.fga_role=owner", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	if len(svc.reqs) != 2 {
		t.Fatalf("expected two explicitly targeted searches, got %+v", svc.reqs)
	}
	for _, got := range svc.reqs {
		if got.FGARole != "owner" {
			t.Fatalf("expected FGA role to be propagated, got %+v", got)
		}
	}
}

func TestCreateRouterSearchRejectsInvalidFGARole(t *testing.T) {
	tests := []struct {
		name  string
		query string
	}{
		{name: "empty", query: "filter.fga_role="},
		{name: "blank", query: "filter.fga_role=+"},
		{name: "multiple", query: "filter.fga_role=owner&filter.fga_role=member"},
		{name: "forbidden character", query: "filter.fga_role=account%3Aowner"},
		{name: "internal whitespace", query: "filter.fga_role=account+owner"},
		{name: "too long", query: "filter.fga_role=" + strings.Repeat("a", 51)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &fakeSearchService{}
			r := CreateRouter(svc, []func(http.Handler) http.Handler{
				withRequestContext(appcontext.RequestContext{Organization: "acme", User: "alice@example.com"}),
			})
			req := httptest.NewRequest(http.MethodGet, "/rest/v1/search?q=hello&"+tt.query, nil)
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, req)

			if rr.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d body=%s", rr.Code, strings.TrimSpace(rr.Body.String()))
			}
			if len(svc.reqs) != 0 {
				t.Fatalf("service must not be called for invalid FGA role: %+v", svc.reqs)
			}
		})
	}
}

func TestCreateRouterResourcesEndpoint(t *testing.T) {
	svc := &fakeSearchService{
		resourcesResp: search.SearchResourcesResponse{
			Resources: []search.SearchResource{
				{Resource: "accounts", DefaultFields: []string{"name"}},
			},
		},
	}
	r := CreateRouter(svc, []func(http.Handler) http.Handler{withRequestContext(appcontext.RequestContext{Organization: "acme", User: "alice@example.com"})})
	req := httptest.NewRequest(http.MethodGet, "/rest/v1/search/resources", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if svc.lastResReq.Organization != "acme" {
		t.Fatalf("unexpected request: %+v", svc.lastResReq)
	}
}

func TestCreateRouterFilterValuesEndpoint(t *testing.T) {
	svc := &fakeSearchService{
		filterValuesResp: search.FilterValuesResponse{Values: []string{"Ready", "Pending"}},
	}
	r := CreateRouter(svc, []func(http.Handler) http.Handler{withRequestContext(appcontext.RequestContext{Organization: "acme", User: "alice@example.com"})})
	req := httptest.NewRequest(http.MethodGet, "/rest/v1/search/filter-values?resource=accounts&field=status&q=foo&filter.type=premium", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if svc.lastFilterReq.Organization != "acme" || svc.lastFilterReq.User != "alice@example.com" {
		t.Fatalf("unexpected request context: %+v", svc.lastFilterReq)
	}
	if svc.lastFilterReq.Resource != "accounts" || svc.lastFilterReq.Field != "status" {
		t.Fatalf("unexpected request payload: %+v", svc.lastFilterReq)
	}
	if len(svc.lastFilterReq.Filters["type"]) != 1 || svc.lastFilterReq.Filters["type"][0] != "premium" {
		t.Fatalf("unexpected filters: %+v", svc.lastFilterReq.Filters)
	}
}

func TestCreateRouterErrorMapping(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		status   int
		category httperr.Category
		problem  string
		detail   string
	}{
		{
			name: "invalid cursor", err: fmt.Errorf("%w: org mismatch", search.ErrInvalidCursor),
			status: http.StatusBadRequest, category: httperr.CategoryInvalidRequest,
			problem: "invalid-cursor", detail: "org mismatch",
		},
		{
			name: "invalid request", err: fmt.Errorf("%w: filters require a resource", search.ErrInvalidRequest),
			status: http.StatusBadRequest, category: httperr.CategoryInvalidRequest,
			problem: "invalid-request", detail: "filters require a resource",
		},
		{
			name: "unauthorized", err: search.ErrUnauthorized,
			status: http.StatusUnauthorized, category: httperr.CategoryAuthentication,
			problem: "authentication-required",
		},
		{
			name: "forbidden", err: search.ErrForbidden,
			status: http.StatusForbidden, category: httperr.CategoryAuthorization,
			problem: "access-denied",
		},
		{
			name: "search backend", err: fmt.Errorf("%w: query OpenSearch: %v", search.ErrSearchBackend, errors.New("connection refused")),
			status: http.StatusInternalServerError, category: httperr.CategorySearchBackend,
			problem: "search-backend-unavailable",
		},
		{
			name: "search backend rejected the query", err: fmt.Errorf("%w: query OpenSearch: %v", search.ErrSearchBackendRejected, errors.New("status 400: number_format_exception")),
			status: http.StatusInternalServerError, category: httperr.CategorySearchBackend,
			problem: "search-query-rejected",
		},
		{
			name: "authorization backend", err: fmt.Errorf("%w: filter authorization: %v", search.ErrAuthzBackend, errors.New("openfga down")),
			status: http.StatusInternalServerError, category: httperr.CategoryAuthorization,
			problem: "authorization-unavailable",
		},
		{
			name: "index unavailable", err: fmt.Errorf("%w: org %q", search.ErrIndexUnavailable, "acme"),
			status: http.StatusInternalServerError, category: httperr.CategorySearchBackend,
			problem: "index-unavailable",
		},
		{
			name: "upstream timeout", err: fmt.Errorf("%w: query OpenSearch: %v", search.ErrUpstreamTimeout, context.DeadlineExceeded),
			status: http.StatusInternalServerError, category: httperr.CategoryUpstreamTimeout,
			problem: "upstream-timeout",
		},
		{
			name: "unclassified", err: errors.New("boom"),
			status: http.StatusInternalServerError, category: httperr.CategoryInternal,
			problem: "internal-error",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := &fakeSearchService{err: tc.err}
			r := CreateRouter(svc, []func(http.Handler) http.Handler{withRequestContext(appcontext.RequestContext{Organization: "acme", User: "alice@example.com"})})
			req := httptest.NewRequest(http.MethodGet, "/rest/v1/search?q=hello", nil)
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, req)

			if rr.Code != tc.status {
				t.Fatalf("expected %d, got %d body=%s", tc.status, rr.Code, strings.TrimSpace(rr.Body.String()))
			}
			if ct := rr.Header().Get("Content-Type"); ct != httperr.ContentType {
				t.Fatalf("expected Content-Type %q, got %q", httperr.ContentType, ct)
			}

			var problem httperr.Problem
			if err := json.Unmarshal(rr.Body.Bytes(), &problem); err != nil {
				t.Fatalf("error body is not JSON: %v (body=%s)", err, rr.Body.String())
			}
			if problem.Category != tc.category {
				t.Fatalf("expected category %q, got %q", tc.category, problem.Category)
			}
			if !strings.HasSuffix(problem.Type, "/"+tc.problem) {
				t.Fatalf("expected type ending in %q, got %q", tc.problem, problem.Type)
			}
			if problem.Status != tc.status {
				t.Fatalf("expected status member %d, got %d", tc.status, problem.Status)
			}
			if problem.Title == "" || problem.Detail == "" {
				t.Fatalf("expected a title and detail, got %+v", problem)
			}
			if tc.detail != "" && problem.Detail != tc.detail {
				t.Fatalf("expected detail %q, got %q", tc.detail, problem.Detail)
			}
			if problem.Instance != "/rest/v1/search" {
				t.Fatalf("expected instance to be the request path, got %q", problem.Instance)
			}
		})
	}
}

// The wrapped cause names the failing backend and must never reach the caller.
func TestCreateRouterErrorDoesNotLeakBackendCause(t *testing.T) {
	causes := []struct {
		name   string
		err    error
		secret string
	}{
		{name: "opensearch", err: fmt.Errorf("%w: query OpenSearch: %v", search.ErrSearchBackend, errors.New("dial tcp 10.1.2.3:9200: connection refused")), secret: "10.1.2.3"},
		{name: "openfga", err: fmt.Errorf("%w: list accessible accounts: %v", search.ErrAuthzBackend, errors.New("no OpenFGA store found")), secret: "OpenFGA"},
		{name: "rejected query", err: fmt.Errorf("%w: query OpenSearch: %v", search.ErrSearchBackendRejected, errors.New(`status 400: {"index":"search-acme-components","reason":"failed to parse field [default_fields.replicas]"}`)), secret: "default_fields.replicas"},
		{name: "unclassified", err: errors.New("panic in kcp resolver: /clusters/root:orgs:acme"), secret: "kcp"},
	}

	for _, tc := range causes {
		t.Run(tc.name, func(t *testing.T) {
			svc := &fakeSearchService{err: tc.err}
			r := CreateRouter(svc, []func(http.Handler) http.Handler{withRequestContext(appcontext.RequestContext{Organization: "acme", User: "alice@example.com"})})
			req := httptest.NewRequest(http.MethodGet, "/rest/v1/search?q=topsecretquery", nil)
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, req)

			body := rr.Body.String()
			if strings.Contains(body, tc.secret) {
				t.Fatalf("error body leaked %q: %s", tc.secret, body)
			}
			if strings.Contains(body, "topsecretquery") {
				t.Fatalf("error body leaked the search query: %s", body)
			}
		})
	}
}

func TestCreateRouterUnknownRouteReturnsProblem(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
		status int
	}{
		{name: "not found", method: http.MethodGet, path: "/rest/v1/nope", status: http.StatusNotFound},
		{name: "method not allowed", method: http.MethodPost, path: "/rest/v1/search", status: http.StatusMethodNotAllowed},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := CreateRouter(&fakeSearchService{}, nil)
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, httptest.NewRequest(tc.method, tc.path, nil))

			if rr.Code != tc.status {
				t.Fatalf("expected %d, got %d", tc.status, rr.Code)
			}
			if ct := rr.Header().Get("Content-Type"); ct != httperr.ContentType {
				t.Fatalf("expected Content-Type %q, got %q", httperr.ContentType, ct)
			}

			var problem httperr.Problem
			if err := json.Unmarshal(rr.Body.Bytes(), &problem); err != nil {
				t.Fatalf("error body is not JSON: %v (body=%s)", err, rr.Body.String())
			}
			if problem.Status != tc.status || problem.Type == "" {
				t.Fatalf("unexpected problem: %+v", problem)
			}
		})
	}
}

func TestCreateRouterInvalidParamsReturnProblem(t *testing.T) {
	tests := []struct {
		name   string
		query  string
		detail string
	}{
		{name: "limit", query: "q=hello&limit=abc", detail: "limit must be an integer"},
		{name: "page", query: "q=hello&page=0", detail: "page must be a positive integer"},
		{name: "filter field", query: "q=hello&filter.=value", detail: "invalid filter field"},
		{name: "filter without resource", query: "q=hello&filter.type=premium", detail: "filtering is not supported when searching across all resources"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := CreateRouter(&fakeSearchService{}, []func(http.Handler) http.Handler{withRequestContext(appcontext.RequestContext{Organization: "acme", User: "alice@example.com"})})
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/rest/v1/search?"+tc.query, nil))

			if rr.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d body=%s", rr.Code, rr.Body.String())
			}

			var problem httperr.Problem
			if err := json.Unmarshal(rr.Body.Bytes(), &problem); err != nil {
				t.Fatalf("error body is not JSON: %v (body=%s)", err, rr.Body.String())
			}
			if problem.Category != httperr.CategoryInvalidRequest {
				t.Fatalf("expected category %q, got %q", httperr.CategoryInvalidRequest, problem.Category)
			}
			if problem.Detail != tc.detail {
				t.Fatalf("expected detail %q, got %q", tc.detail, problem.Detail)
			}
		})
	}
}

func TestCreateRouterResourcesParam(t *testing.T) {
	svc := &fakeSearchService{}
	r := CreateRouter(svc, []func(http.Handler) http.Handler{withRequestContext(appcontext.RequestContext{Organization: "acme", User: "alice@example.com"})})
	req := httptest.NewRequest(http.MethodGet, "/rest/v1/search?q=hello&resources=accounts,+components+,", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	// Only the listed resources are searched (blanks trimmed); ListResources is never consulted.
	searched := make([]string, 0, len(svc.reqs))
	for _, req := range svc.reqs {
		searched = append(searched, req.Resource)
	}
	if len(searched) != 2 {
		t.Fatalf("expected 2 resources searched, got %v", searched)
	}
	if svc.lastResReq.Organization != "" {
		t.Fatalf("expected ListResources not to be called")
	}

	// Multiple resources produce a keyed result map.
	var payload map[string]search.SearchResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not valid json: %v", err)
	}
	if _, ok := payload["accounts"]; !ok {
		t.Fatalf("missing accounts in response: %v", payload)
	}
	if _, ok := payload["components"]; !ok {
		t.Fatalf("missing components in response: %v", payload)
	}
}

func TestCreateRouterResourceParamTakesPrecedence(t *testing.T) {
	svc := &fakeSearchService{}
	r := CreateRouter(svc, []func(http.Handler) http.Handler{withRequestContext(appcontext.RequestContext{Organization: "acme", User: "alice@example.com"})})
	req := httptest.NewRequest(http.MethodGet, "/rest/v1/search?q=hello&resource=accounts&resources=components,services", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if len(svc.reqs) != 1 || svc.reqs[0].Resource != "accounts" {
		t.Fatalf("expected single search for accounts, got %v", svc.reqs)
	}
}

func TestCORSPreflightReturns200(t *testing.T) {
	paths := []string{
		"/rest/v1/search",
		"/rest/v1/search/resources",
		"/rest/v1/search/filter-values",
	}
	r := CreateRouter(&fakeSearchService{}, nil)
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodOptions, path, nil)
			req.Header.Set("Origin", "https://example.com")
			req.Header.Set("Access-Control-Request-Method", "GET")
			r.ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("expected 200 for OPTIONS %s, got %d", path, rr.Code)
			}
		})
	}
}

func TestErrorContractAcrossEndpoints(t *testing.T) {
	for _, path := range []string{"/rest/v1/search?resource=accounts", "/rest/v1/search/resources", "/rest/v1/search/filter-values?resource=accounts&field=name"} {
		t.Run(path, func(t *testing.T) {
			cause := fmt.Errorf("%w: secret backend query", search.ErrSearchBackend)
			svc := &fakeSearchService{err: cause, resourcesErr: cause, filterValuesErr: cause}
			r := CreateRouter(svc, []func(http.Handler) http.Handler{withRequestContext(appcontext.RequestContext{Organization: "acme", User: "alice"})})
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
			var p httperr.Problem
			if err := json.Unmarshal(rr.Body.Bytes(), &p); err != nil {
				t.Fatal(err)
			}
			if rr.Code != 500 || p.Status != 500 || p.Category != httperr.CategorySearchBackend || p.Type == "" || p.Title == "" || rr.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("unexpected error contract: %d %s", rr.Code, rr.Body.String())
			}
			if strings.Contains(rr.Body.String(), "secret") {
				t.Fatal("backend details leaked")
			}
		})
	}
}
