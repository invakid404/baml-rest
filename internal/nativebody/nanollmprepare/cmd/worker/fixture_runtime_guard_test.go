//go:build !debamlworkerfixture

package main

// De-BAML serving cutover S3a — the guard that keeps the build fixture OUT of every
// shipped build of this entrypoint.
//
// fixture_runtime.go gives this binary a real `Baml_Rest_Dynamic` method table when
// built with `debamlworkerfixture`, so the booted-artifact proof has something to send
// a request to. That is build-fixture surface, and the whole argument for it being
// safe is that a shipped build cannot reach it. This file is that argument, checked:
// it exists only in the UNTAGGED build, and it requires the untagged build to install
// no runtime override at all.
//
// If the tagged and untagged halves of the pair are ever swapped, or the default half
// starts returning something, the shipped worker would dispatch through a method table
// nobody deployed — and this goes red.

import (
	"reflect"
	"testing"

	"github.com/invakid404/baml-rest/internal/workerboot"
)

// TestShippedEntrypointsInstallNoRuntimeOverride pins the default half of the pair.
func TestShippedEntrypointsInstallNoRuntimeOverride(t *testing.T) {
	if got := fixtureRuntime(); got != nil {
		t.Fatalf("a build without the fixture tag installs runtime override %T; a shipped worker must dispatch through the root generated package the container build wrote", got)
	}
	for name, opts := range map[string]workerboot.Options{
		"flag off": flagOffProfileOptions(),
		"serve":    serveProfileOptions(),
	} {
		if opts.Runtime != nil {
			t.Errorf("%s profile installs runtime override %T; Options.Runtime must be nil on every shipped entrypoint", name, opts.Runtime)
		}
	}
}

// TestFlagOffProfileInstallsNoNativeFieldAtAll is the OTHER half of the fixture's
// safety argument, and the flag-off zero-native contract checked at the VALUE level.
//
// The S2 entrypoint guard exempts Options.Runtime from its source-level
// native-injection classification, because a method table is not native wiring. That
// exemption is only safe if the flag-off profile still installs nothing native — so
// this asserts it on the value the entrypoint actually returns, which no source-level
// exemption can affect.
//
// It is DERIVED, not hand-listed. An earlier revision enumerated the factories it knew
// about and silently omitted NativeShadowFactory and NativeStaticShadowFactory; a
// hand-list is a guard you can forget to extend, and forgetting is exactly the failure
// mode here. Reflection over workerboot.Options means a NEW native field is covered the
// moment it exists, and TestFlagOffFieldGuardFiresForEveryNativeField below proves the
// predicate really reports each one.
func TestFlagOffProfileInstallsNoNativeFieldAtAll(t *testing.T) {
	names := nativeInjectionFieldNames(t)
	off := reflect.ValueOf(flagOffProfileOptions())
	for _, name := range names {
		if !off.FieldByName(name).IsNil() {
			t.Errorf("flag-off profile installs %s; BAML_REST_USE_DEBAML=false must install no capability, no runtime init and no factory", name)
		}
	}
	if !flagOffProfileOptions().NativeBuildCapable {
		t.Error("flag-off profile stopped advertising the static build capability; the artifact would contradict its own stamp and refuse to serve")
	}
	// Non-vacuity: the flag-ON profile really does install native machinery, so the
	// absence above is a statement about a worker that HAS a native lane to suppress.
	on := reflect.ValueOf(serveProfileOptions())
	installed := 0
	for _, name := range names {
		if !on.FieldByName(name).IsNil() {
			installed++
		}
	}
	if installed == 0 {
		t.Error("the serve profile installs no native field at all; the flag-off assertion above would be vacuous")
	}
}

// TestFlagOffFieldGuardFiresForEveryNativeField is the BITE for the predicate above.
// An assertion that every native field is nil is worth exactly as much as its ability
// to notice a field that is NOT — so this sets each discovered field, one at a time, on
// an otherwise flag-off options value and requires the predicate to report that field.
//
// This is what makes "you cannot remove a guard" true: there is no per-field guard to
// remove, and the one predicate is proven to fire for every field it covers.
func TestFlagOffFieldGuardFiresForEveryNativeField(t *testing.T) {
	names := nativeInjectionFieldNames(t)
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			opts := flagOffProfileOptions()
			v := reflect.ValueOf(&opts).Elem().FieldByName(name)
			v.Set(nonNilFor(t, name, v.Type()))

			var reported []string
			probe := reflect.ValueOf(opts)
			for _, n := range names {
				if !probe.FieldByName(n).IsNil() {
					reported = append(reported, n)
				}
			}
			if len(reported) != 1 || reported[0] != name {
				t.Errorf("setting %s was reported as %v; the flag-off predicate must notice exactly the field that was installed", name, reported)
			}
		})
	}
}

// nativeInjectionFieldNames is the same classification the S2 source-level entrypoint
// guard applies (internal/workerboot/native_entrypoint_profile_test.go), restated here
// so this value-level check cannot cover a smaller set than that one: every
// Func- or Interface-kinded field on workerboot.Options can carry native machinery.
//
// Runtime is the single exemption, for the reason the S2 guard documents: it is the
// BAML method table, not native wiring, and TestShippedEntrypointsInstallNoRuntimeOverride
// pins it nil on every shipped build anyway.
//
// It FAILS CLOSED on a field shape it has not reasoned about, exactly as the S2 guard
// does — a new field kind could carry an engine, and silently treating it as inert is
// the gap this whole file exists to close.
func nativeInjectionFieldNames(t *testing.T) []string {
	t.Helper()
	var out []string
	typ := reflect.TypeOf(workerboot.Options{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.Name == "Runtime" {
			continue
		}
		switch f.Type.Kind() {
		case reflect.Func, reflect.Interface:
			out = append(out, f.Name)
		case reflect.Bool, reflect.String:
			// Static advertisement only; inert in the flag-off literal.
		default:
			t.Fatalf("workerboot.Options field %s has unclassified kind %s; extend this guard before adding it", f.Name, f.Type.Kind())
		}
	}
	if len(out) == 0 {
		t.Fatal("no native injection fields discovered on workerboot.Options; this guard would be vacuous")
	}
	return out
}

// stubNativeCapability is a non-nil worker.NativeCapability for the bite above. It
// touches no FFI: the bite is about the PREDICATE noticing a populated field, not about
// what a real capability does.
type stubNativeCapability struct{}

func (stubNativeCapability) NativeEngine() string        { return "stub" }
func (stubNativeCapability) NativeEngineVersion() string { return "" }

// nonNilFor builds a non-nil value of typ for the bite. Func fields get a synthetic
// implementation; the one interface field gets the stub above.
func nonNilFor(t *testing.T, name string, typ reflect.Type) reflect.Value {
	t.Helper()
	switch typ.Kind() {
	case reflect.Func:
		return reflect.MakeFunc(typ, func(args []reflect.Value) []reflect.Value {
			out := make([]reflect.Value, typ.NumOut())
			for i := range out {
				out[i] = reflect.Zero(typ.Out(i))
			}
			return out
		})
	case reflect.Interface:
		stub := reflect.ValueOf(stubNativeCapability{})
		if !stub.Type().Implements(typ) {
			t.Fatalf("field %s is an interface this bite cannot populate (%s); add a stub for it", name, typ)
		}
		return stub
	default:
		t.Fatalf("field %s has kind %s, which this bite cannot populate", name, typ.Kind())
		return reflect.Value{}
	}
}
