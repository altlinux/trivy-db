package alt

import (
	"encoding/json"
	"os"
	"path/filepath"

	"golang.org/x/xerrors"
)

type resolvedTest struct {
	Name string
	//SignatureKeyID signatureKeyID
	FixedVersion string
	Arch         string
}

func unmarshalJSONFile(v interface{}, fileName string) error {
	f, err := os.Open(fileName)
	if err != nil {
		return xerrors.Errorf("unable to open a file (%s): %w", fileName, err)
	}
	defer f.Close()

	if err = json.NewDecoder(f).Decode(v); err != nil {
		return xerrors.Errorf("failed to decode ALT OVAL JSON: %w", err)
	}
	return nil
}

func parseObjects(dir string) (map[string]RPMInfoObject, error) {
	var objects Objects
	if err := unmarshalJSONFile(&objects, filepath.Join(dir, "objects.json")); err != nil {
		return nil, xerrors.Errorf("failed to unmarshal objects: %w", err)
	}

	objs := map[string]RPMInfoObject{}
	for _, obj := range objects.RPMInfoObjects {
		objs[obj.ID] = obj
	}

	return objs, nil
}

func parseStates(dir string) (map[string]RPMInfoState, error) {
	var ss States
	if err := unmarshalJSONFile(&ss, filepath.Join(dir, "states.json")); err != nil {
		return nil, xerrors.Errorf("failed to unmarshal states: %w", err)
	}

	states := map[string]RPMInfoState{}
	for _, state := range ss.RPMInfoStates {
		states[state.ID] = state
	}
	return states, nil
}

func parseTests(dir string) (Tests, error) {
	var tests Tests
	if err := unmarshalJSONFile(&tests, filepath.Join(dir, "tests.json")); err != nil {
		return Tests{}, xerrors.Errorf("failed to unmarshal tests: %w", err)
	}

	return tests, nil
}

func resolveTests(dir string) (map[string]resolvedTest, error) {
	objects, err := parseObjects(dir)
	if err != nil {
		return nil, err
	}

	states, err := parseStates(dir)
	if err != nil {
		return nil, err
	}

	tests, err := parseTests(dir)
	if err != nil {
		return nil, err
	}

	rpmTests := map[string]resolvedTest{}
	for _, test := range tests.RPMInfoTests {
		t, err := followTestRefs(test, objects, states)
		if err != nil {
			return nil, err
		}
		rpmTests[test.ID] = t
	}
	return rpmTests, nil
}

func parseDefinitions(dir string) (Definitions, error) {
	var definitions Definitions

	if err := unmarshalJSONFile(&definitions, filepath.Join(dir, "definitions.json")); err != nil {
		return Definitions{}, xerrors.Errorf("failed to parse definitions: %w", err)
	}
	return definitions, nil
}

func followTestRefs(test RPMInfoTest, objects map[string]RPMInfoObject, states map[string]RPMInfoState) (resolvedTest, error) {
	var t resolvedTest

	// Follow object ref
	if test.Object.ObjectRef == "" {
		return t, nil
	}

	obj, ok := objects[test.Object.ObjectRef]
	if !ok {
		return t, xerrors.Errorf("invalid tests data, can't find object ref: %s, test ref: %s",
			test.Object.ObjectRef, test.ID)
	}
	t.Name = obj.Name

	// Follow state ref
	if test.State.StateRef == "" {
		return t, nil
	}

	state, ok := states[test.State.StateRef]
	if !ok {
		return t, xerrors.Errorf("invalid tests data, can't find ovalstate ref %s, test ref: %s",
			test.State.StateRef, test.ID)
	}

	if state.Arch.Datatype == "string" && (state.Arch.Operation == "pattern match" || state.Arch.Operation == "equals") {
		t.Arch = state.Arch.Text
	}

	if state.EVR.Datatype == "evr_string" && state.EVR.Operation == "less than" {
		t.FixedVersion = state.EVR.Text
	}

	return t, nil
}
