package destination

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestValidateOptions(t *testing.T) {
	one, ten := 0, 28
	specs := []OptionSpec{
		{Key: "gate", Type: OptionEnum, Enum: []EnumValue{{Value: ""}, {Value: "nobody"}}},
		{Key: "who", Type: OptionEnumList, Enum: []EnumValue{{Value: "mention"}, {Value: "following"}}},
		{Key: "quotes_off", Type: OptionBool},
		{Key: "pow", Type: OptionInt, Min: &one, Max: &ten},
		{Key: "tag", Type: OptionString},
	}
	var good map[string]any
	_ = json.Unmarshal([]byte(`{"gate":"nobody","who":["mention"],"quotes_off":true,"pow":20,"tag":"go","x":null}`), &good)
	delete(good, "x")
	if errs := ValidateOptions(specs, good); errs != nil {
		t.Fatalf("valid options rejected: %v", errs)
	}
	var bad map[string]any
	_ = json.Unmarshal([]byte(`{"gate":"some","who":["mention","bogus"],"quotes_off":"yes","pow":29,"tag":3,"typo":1}`), &bad)
	want := []string{
		`gate: must be one of "", "nobody"`,
		"pow: must be at most 28",
		"quotes_off: must be true or false",
		"tag: must be a string",
		"typo: unknown option",
		`who: each value must be one of "mention", "following"`,
	}
	if got := ValidateOptions(specs, bad); !reflect.DeepEqual(got, want) {
		t.Fatalf("errors =\n%q\nwant\n%q", got, want)
	}
}

func TestOptAccessors(t *testing.T) {
	var o map[string]any
	_ = json.Unmarshal([]byte(`{"s":"x","b":true,"n":3,"l":["a","b"]}`), &o)
	if OptString(o, "s") != "x" || !OptBool(o, "b") || OptString(o, "missing") != "" {
		t.Error("string/bool accessors")
	}
	if n, ok := OptInt(o, "n"); !ok || n != 3 {
		t.Error("int accessor")
	}
	if _, ok := OptInt(o, "missing"); ok {
		t.Error("missing int must report unset")
	}
	if !reflect.DeepEqual(OptStrings(o, "l"), []string{"a", "b"}) {
		t.Error("list accessor")
	}
}

type fakeDriver struct{ m Manifest }

func (f fakeDriver) Manifest() Manifest { return f.m }

func (f fakeDriver) Open(context.Context, OpenRequest) (Account, error) {
	return nil, errors.New("fake driver opens nothing")
}

func TestRegistry(t *testing.T) {
	Register(fakeDriver{Manifest{Contract: ContractVersion, Type: "zz_test"}})
	defer Unregister("zz_test")
	if _, ok := Lookup("zz_test"); !ok {
		t.Fatal("registered driver not found")
	}
	mustPanic := func(name string, f func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Errorf("%s: expected panic", name)
			}
		}()
		f()
	}
	mustPanic("duplicate", func() { Register(fakeDriver{Manifest{Contract: ContractVersion, Type: "zz_test"}}) })
	mustPanic("bad type", func() { Register(fakeDriver{Manifest{Contract: ContractVersion, Type: "Bad-Type"}}) })
	mustPanic("bad contract", func() { Register(fakeDriver{Manifest{Contract: 99, Type: "zz_other"}}) })
	// A driver written against contract 1 (no Open) is refused.
	mustPanic("contract 1", func() { Register(fakeDriver{Manifest{Contract: 1, Type: "zz_old"}}) })
}

// Types are listed in registration order, which is the order the composer
// shows them in; Types() stays sorted.
func TestTypesInOrder(t *testing.T) {
	for _, typ := range []string{"zz_c", "zz_a", "zz_b"} {
		Register(fakeDriver{Manifest{Contract: ContractVersion, Type: typ}})
		defer Unregister(typ)
	}
	var got []string
	for _, typ := range TypesInOrder() {
		if len(typ) > 3 && typ[:3] == "zz_" {
			got = append(got, typ)
		}
	}
	if want := []string{"zz_c", "zz_a", "zz_b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("TypesInOrder = %v, want %v", got, want)
	}
	Unregister("zz_a")
	got = got[:0]
	for _, typ := range TypesInOrder() {
		if len(typ) > 3 && typ[:3] == "zz_" {
			got = append(got, typ)
		}
	}
	if want := []string{"zz_c", "zz_b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after Unregister: %v, want %v", got, want)
	}
}
