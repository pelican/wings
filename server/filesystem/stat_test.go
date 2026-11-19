package filesystem

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

func TestStatCTime(t *testing.T) {
	fs := NewFs()
	defer func() { _ = fs.TruncateRootDirectory() }()

	r := bytes.NewReader([]byte("hello"))

	if err := fs.Writefile("ctime_test.txt", r); err != nil {
		t.Fatal(err)
	}
	// if err := fs.CreateServerFileFromString("ctime_test.txt", "hello"); err != nil {
	// 	t.Fatal(err)
	// }

	st, err := fs.Stat("ctime_test.txt")
	if err != nil {
		t.Fatal(err)
	}

	ctime := st.CTime()
	if ctime.IsZero() {
		t.Error("expected non-zero CTime")
	}
	if time.Since(ctime) > 10*time.Second {
		t.Errorf("CTime seems too old: %v", ctime)
	}
}

func TestStatMarshalJSON(t *testing.T) {
	fs := NewFs()
	defer func() { _ = fs.TruncateRootDirectory() }()

	r := bytes.NewReader([]byte("hello world"))

	if err := fs.Writefile("json_test.txt", r); err != nil {
		t.Fatal(err)
	}

	// if err := fs.CreateServerFileFromString("json_test.txt", "hello world"); err != nil {
	// 	t.Fatal(err)
	// }

	st, err := fs.Stat("json_test.txt")
	if err != nil {
		t.Fatal(err)
	}

	b, err := json.Marshal(&st)
	if err != nil {
		t.Fatal(err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(b, &result); err != nil {
		t.Fatal(err)
	}

	created, ok := result["created"].(string)
	if !ok || created == "" {
		t.Error("expected 'created' field in JSON output")
	}

	if _, err := time.Parse(time.RFC3339, created); err != nil {
		t.Errorf("expected RFC3339 timestamp for 'created', got %q: %v", created, err)
	}

	modified, ok := result["modified"].(string)
	if !ok || modified == "" {
		t.Error("expected 'modified' field in JSON output")
	}

	name, ok := result["name"].(string)
	if !ok || name != "json_test.txt" {
		t.Errorf("expected name 'json_test.txt', got %q", name)
	}
}
