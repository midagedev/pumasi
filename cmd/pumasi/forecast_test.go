package main

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/midagedev/pumasi/internal/controller"
	"github.com/midagedev/pumasi/internal/watch"
)

// `pumasi forecast --demo` and the demo dashboard's /api/forecast say the
// same: the same cores, from the demo's hosts, and the same weeks to fill.
func TestDemoForecastMatchesDashboard(t *testing.T) {
	cli, err := local(&source{demo: true}, controller.DemoCores(), 8)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(controller.NewDemo(1).Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/forecast")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var api watch.ForecastReport
	if err := json.NewDecoder(resp.Body).Decode(&api); err != nil {
		t.Fatal(err)
	}
	if cli.Cores != api.Cores || cli.CoresFrom != api.CoresFrom {
		t.Fatalf("cores: cli %.0f (%s), api %.0f (%s)", cli.Cores, cli.CoresFrom, api.Cores, api.CoresFrom)
	}
	// Made a moment apart, the two may differ in the last digits.
	if math.Abs(cli.Load*cli.Cores-api.Load*api.Cores) > 0.5 || math.Abs(cli.FullIn-api.FullIn) > 0.5 || api.FullIn <= 0 {
		t.Fatalf("cli: %s\napi: %s", cli.Words, api.Words)
	}
}
