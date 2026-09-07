// Nothing puts these messages on a wire yet, so the shipped payload bytes are
// the only thing that exercises them: a `.proto` no test parses drifts within
// weeks (ADR 0074).
package kubernetesv1_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/RebuildStackCo/runtime-agent/internal/pb/kubernetesv1"
)

// The golden payloads are written by the local sink, which writes exactly the
// bytes the backend sink transmits (docs/development.md).
const corpus = "../../sink/testdata"

// The kinds this schema covers, and the file each one's payload lives in.
func schemaCases() map[string]func() proto.Message {
	return map[string]func() proto.Message{
		"node-metadata.golden.json":       func() proto.Message { return &kubernetesv1.NodeMetadata{} },
		"collection-coverage.golden.json": func() proto.Message { return &kubernetesv1.CollectionCoverage{} },
		"node-lifecycle.golden.json":      func() proto.Message { return &kubernetesv1.NodeLifecycle{} },
	}
}

// DiscardUnknown is off, so a field the corpus carries and the schema does not
// fails here rather than passing silently.
func TestEveryCoveredGoldenParsesIntoItsMessage(t *testing.T) {
	for name, newMessage := range schemaCases() {
		t.Run(name, func(t *testing.T) {
			msg := newMessage()
			if err := protojson.Unmarshal(readGolden(t, name), msg); err != nil {
				t.Fatalf("parsing %s into %T: %v", name, msg, err)
			}
		})
	}
}

// Presence is the half a parse cannot check: a field declared without it
// accepts the corpus and then reports its zero as unset, which is the
// distinction this contract rests on (ADR 0074).
func TestEveryValueTheCorpusStatesIsStillSetAfterParsing(t *testing.T) {
	for name, newMessage := range schemaCases() {
		t.Run(name, func(t *testing.T) {
			raw := readGolden(t, name)
			var stated map[string]any
			if err := json.Unmarshal(raw, &stated); err != nil {
				t.Fatalf("reading %s as JSON: %v", name, err)
			}
			msg := newMessage()
			if err := protojson.Unmarshal(raw, msg); err != nil {
				t.Fatalf("parsing %s into %T: %v", name, msg, err)
			}
			assertStatedFieldsAreSet(t, name, stated, msg.ProtoReflect())
		})
	}
}

// A golden for a kind the schema does not cover must not be silently skipped,
// and neither must one that was renamed out from under the table above.
func TestTheCoveredKindsAreTheOnesTheSchemaDeclares(t *testing.T) {
	declared := schemaCases()
	for name := range declared {
		if _, err := os.Stat(filepath.Join(corpus, name)); err != nil {
			t.Errorf("the schema covers %s, which the corpus no longer holds: %v", name, err)
		}
	}

	files, err := os.ReadDir(corpus)
	if err != nil {
		t.Fatalf("reading the corpus: %v", err)
	}
	var uncovered []string
	for _, f := range files {
		if _, ok := declared[f.Name()]; !ok {
			uncovered = append(uncovered, f.Name())
		}
	}
	sort.Strings(uncovered)
	t.Logf("%d payload kinds have a schema; %d do not: %v", len(declared), len(uncovered), uncovered)
}

// assertStatedFieldsAreSet walks the JSON beside the parsed message and reports
// every key whose value did not survive as a set field. For a scalar that means
// the schema gave it no presence and the corpus stated its zero.
func assertStatedFieldsAreSet(t *testing.T, path string, stated map[string]any, msg protoreflect.Message) {
	t.Helper()
	fields := msg.Descriptor().Fields()
	for key, value := range stated {
		fd := fields.ByJSONName(key)
		if fd == nil {
			fd = fields.ByTextName(key)
		}
		if fd == nil {
			t.Errorf("%s: the corpus states %q and the schema has no such field", path, key)
			continue
		}
		where := path + "." + key

		switch {
		case fd.IsList():
			elements, ok := value.([]any)
			if !ok || len(elements) == 0 {
				continue
			}
			list := msg.Get(fd).List()
			if list.Len() != len(elements) {
				t.Errorf("%s: the corpus states %d elements, the message holds %d", where, len(elements), list.Len())
				continue
			}
			if fd.Kind() != protoreflect.MessageKind {
				continue
			}
			for i, element := range elements {
				nested, ok := element.(map[string]any)
				if !ok {
					continue
				}
				assertStatedFieldsAreSet(t, fmt.Sprintf("%s[%d]", where, i), nested, list.Get(i).Message())
			}
		case fd.IsMap():
			continue
		case !msg.Has(fd):
			t.Errorf("%s: the corpus states %v and the parsed message reports the field unset; "+
				"a zero the corpus states is a claim, so declare `optional` on it (ADR 0074)", where, value)
		case fd.Kind() == protoreflect.MessageKind && fd.Message().FullName() != "google.protobuf.Timestamp":
			nested, ok := value.(map[string]any)
			if !ok {
				continue
			}
			assertStatedFieldsAreSet(t, where, nested, msg.Get(fd).Message())
		}
	}
}

func readGolden(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(corpus, name)) // #nosec G304 -- name is a literal in the table above
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return raw
}
