package baml_rest

import (
	"context"

	v0240 "github.com/invakid404/baml-rest/adapters/v0.204.0"
	"github.com/invakid404/baml-rest/bamlutils"
)

// NOTE: this file will be overwritten during build

var Methods = map[string]bamlutils.StreamingMethod{}

func MakeAdapter(ctx context.Context) bamlutils.Adapter {
	return &v0240.BamlAdapter{Context: ctx}
}
