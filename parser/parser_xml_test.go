package parser

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// An XML file nested deeper than maxXMLDepth is left as it is.
func TestNestedXMLOverDepthLimitIsNotRewritten(t *testing.T) {
	const depth = 10000
	content := strings.Repeat("<a>", depth) + strings.Repeat("</a>", depth)
	path := filepath.Join(t.TempDir(), "config.xml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	var cf ConfigurationFile
	require.NoError(t, json.Unmarshal([]byte(`{"file":"config.xml","parser":"xml","replace":[{"match":"a","replace_with":"value"}]}`), &cf))
	cf.configuration = []byte(`{}`)

	f, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(t, err)
	defer f.Close()

	start := time.Now()
	require.Error(t, cf.parseXmlFile(f))
	require.Less(t, time.Since(start), 5*time.Second)

	b, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, content, string(b))
}

// fullFile is a configuration file that reports there is no room for it to grow.
type fullFile struct {
	*os.File
}

func (fullFile) CheckSize(int64) error {
	return os.ErrPermission
}

// A rewrite that would not fit within the disk limit leaves the file unchanged,
// rather than truncating it and then failing part way through writing it.
func TestRewriteThatDoesNotFitLeavesFileUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.properties")
	require.NoError(t, os.WriteFile(path, []byte("motd=hello\n"), 0o644))

	f, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(t, err)
	defer f.Close()

	require.Error(t, rewrite(fullFile{f}, []byte("motd=a much longer message of the day\n")))

	b, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "motd=hello\n", string(b))
}
