package store

import (
	"reflect"
	"testing"
)

// StoreMethodCount is the number of methods the Store interface requires.
//
// It is asserted rather than merely observed because of a failure mode
// the compiler cannot see. Store is composed of six embedded
// sub-interfaces, and if a method is DROPPED from one of them ,
// during a split, a merge, or a careless edit, nothing breaks. Every
// implementation still has the method, the interface simply stops
// requiring it, the package compiles, and the whole suite passes. A
// requirement disappears in silence.
//
// The opposite mistake is free: a method declared in two embedded
// interfaces is a duplicate-method compile error. So only the dropped
// case needs a guard, and this is it.
//
// WHEN THIS TEST FAILS, DO NOT JUST UPDATE THE NUMBER. Work out which
// method appeared or vanished and whether that was intended. Adding a
// store method should bump this deliberately, in the same commit, as an
// acknowledgement that the persistence contract changed. A number
// updated reflexively to make a test green defeats the entire point of
// having it.
//
// 259 was the count on 2026-09-04, verified against the flat Store
// interface immediately before it was split into sub-interfaces.
const StoreMethodCount = 259

func TestStoreInterfaceIsComplete(t *testing.T) {
	got := reflect.TypeOf((*Store)(nil)).Elem().NumMethod()
	if got != StoreMethodCount {
		t.Fatalf(
			"Store requires %d methods, expected %d.\n\n"+
				"If you ADDED a store method, bump StoreMethodCount in this file in "+
				"the same commit.\n"+
				"If you did not add one, a method has been dropped from an embedded "+
				"sub-interface. That does not break the build, the implementations "+
				"still have it and the interface just stopped requiring it, so this "+
				"test is the only thing that will tell you.",
			got, StoreMethodCount)
	}
}

// The sub-interfaces must actually be reachable through Store. A file
// that declared one but forgot to embed it would compile, pass the count
// check only by coincidence, and leave its methods unrequired.
func TestStoreEmbedsEverySubInterface(t *testing.T) {
	store := reflect.TypeOf((*Store)(nil)).Elem()

	subs := []struct {
		name string
		typ  reflect.Type
	}{
		{"IdentityStore", reflect.TypeOf((*IdentityStore)(nil)).Elem()},
		{"ProjectStore", reflect.TypeOf((*ProjectStore)(nil)).Elem()},
		{"ExecutionStore", reflect.TypeOf((*ExecutionStore)(nil)).Elem()},
		{"DetectionStore", reflect.TypeOf((*DetectionStore)(nil)).Elem()},
		{"BillingLifecycleStore", reflect.TypeOf((*BillingLifecycleStore)(nil)).Elem()},
		{"CheckpointStore", reflect.TypeOf((*CheckpointStore)(nil)).Elem()},
	}

	var total int
	for _, s := range subs {
		if !store.Implements(s.typ) {
			t.Errorf("Store does not satisfy %s; it is declared but not embedded, "+
				"so none of its methods are required", s.name)
		}
		if s.typ.NumMethod() == 0 {
			t.Errorf("%s declares no methods, which means its contents were lost", s.name)
		}
		total += s.typ.NumMethod()
	}

	// Every sub-interface method, plus Store's own lifecycle methods
	// (Close, Ping, SchemaStatus), must add up to the whole. A gap here
	// means methods live in a sub-interface that Store does not reach,
	// or in Store directly when they belong in a sub-interface.
	const lifecycleMethods = 3
	if want := total + lifecycleMethods; want != StoreMethodCount {
		t.Errorf("the sub-interfaces contribute %d methods and Store declares %d "+
			"lifecycle methods, totalling %d, but Store requires %d. Some methods "+
			"are unaccounted for",
			total, lifecycleMethods, want, StoreMethodCount)
	}
}
