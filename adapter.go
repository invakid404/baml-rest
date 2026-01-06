package baml_rest

import (
	"context"

	_ "github.com/enriquebris/goconcurrentqueue"

	"github.com/invakid404/baml-rest/bamlutils"
)

// NOTE: this file will be overwritten during build

var Methods = map[string]bamlutils.StreamingMethod{}

func MakeAdapter(context.Context) bamlutils.Adapter {
	return (bamlutils.Adapter)(nil)
}
