package stub_test

import (
	"testing"

	"github.com/kameas-ai/kenaz-plugin/fleetapi"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/fleetapitest"
	"github.com/kameas-ai/kenaz-plugin/fleetapi/stub"
)

func TestStubConformance(t *testing.T) {
	p := stub.New()
	if p.Flavor() != fleetapi.FlavorUnofficial {
		t.Fatalf("stub flavor = %q", p.Flavor())
	}
	fleetapitest.Run(t, p, fleetapitest.Options{})
}
