package codegen

import "testing"

type benchOption func()

var benchOptionsSink []benchOption

type benchAdapter struct {
	clientRegistry *struct{}
	typeBuilder    *struct{}
}

func benchWithClientRegistry(*struct{}) benchOption { return func() {} }
func benchWithTypeBuilder(*struct{}) benchOption    { return func() {} }
func benchWithOnTick(benchOption) benchOption       { return func() {} }

// oldMakeOptions simulates the old codegen pattern: nil slice, no pre-sizing.
func oldMakeOptions(adapterIn any) ([]benchOption, error) {
	adapter, ok := adapterIn.(*benchAdapter)
	if !ok {
		panic("oldMakeOptions: unexpected adapter type")
	}

	var result []benchOption
	if adapter.clientRegistry != nil {
		result = append(result, benchWithClientRegistry(adapter.clientRegistry))
	}
	if adapter.typeBuilder != nil {
		result = append(result, benchWithTypeBuilder(adapter.typeBuilder))
	}
	return result, nil
}

// oldAssembleCallOptions simulates the old pattern: build a temporary
// []benchOption{WithOnTick(onTick)} slice, then append options to it.
func oldAssembleCallOptions(adapter *benchAdapter, onTick benchOption) []benchOption {
	options, _ := oldMakeOptions(adapter)
	return append(
		[]benchOption{benchWithOnTick(onTick)},
		options...,
	)
}

// newMakeOptions simulates the new codegen pattern: pre-sized with cap 3.
func newMakeOptions(adapterIn any) ([]benchOption, error) {
	adapter, ok := adapterIn.(*benchAdapter)
	if !ok {
		panic("newMakeOptions: unexpected adapter type")
	}

	result := make([]benchOption, 0, 3)
	if adapter.clientRegistry != nil {
		result = append(result, benchWithClientRegistry(adapter.clientRegistry))
	}
	if adapter.typeBuilder != nil {
		result = append(result, benchWithTypeBuilder(adapter.typeBuilder))
	}
	return result, nil
}

// newAssembleCallOptions simulates the new pattern: append WithOnTick
// into the spare capacity of the pre-sized options slice.
func newAssembleCallOptions(adapter *benchAdapter, onTick benchOption) []benchOption {
	options, _ := newMakeOptions(adapter)
	return append(options, benchWithOnTick(onTick))
}

func BenchmarkGeneratedOptionsAssembly_Current(b *testing.B) {
	adapter := &benchAdapter{
		clientRegistry: &struct{}{},
		typeBuilder:    &struct{}{},
	}
	onTick := func() {}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		benchOptionsSink = oldAssembleCallOptions(adapter, onTick)
	}
}

func BenchmarkGeneratedOptionsAssembly_Presized(b *testing.B) {
	adapter := &benchAdapter{
		clientRegistry: &struct{}{},
		typeBuilder:    &struct{}{},
	}
	onTick := func() {}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		benchOptionsSink = newAssembleCallOptions(adapter, onTick)
	}
}
