package parser

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/beevik/etree"
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

// A small XML file whose indentation would expand to far more than the size
// limit is refused before that whitespace is ever allocated, so a crafted file
// cannot exhaust daemon memory. The file is left unchanged, like one over the
// size or depth limits.
func TestXMLWithExpensiveIndentationIsNotRewritten(t *testing.T) {
	// A shallow chain (well under maxXMLDepth) holding very many leaf elements:
	// tiny on disk, but its indentation whitespace alone dwarfs the size limit.
	const (
		depth    = 300
		children = 200000
	)
	content := strings.Repeat("<a>", depth) + strings.Repeat("<x/>", children) + strings.Repeat("</a>", depth)
	require.Less(t, len(content), maxConfigFileSize, "the crafted input must itself be under the size limit")

	path := filepath.Join(t.TempDir(), "config.xml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	var cf ConfigurationFile
	require.NoError(t, json.Unmarshal([]byte(`{"file":"config.xml","parser":"xml","replace":[{"match":"a","replace_with":"value"}]}`), &cf))
	cf.configuration = []byte(`{}`)

	f, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(t, err)
	defer f.Close()

	start := time.Now()
	err = cf.parseXmlFile(f)
	require.Error(t, err)
	// The pre-indentation guard must be what rejects it, before Indent ever
	// allocates the whitespace — not the size check on an already-built buffer.
	require.Contains(t, err.Error(), "indented result exceeds limit")
	require.Less(t, time.Since(start), 5*time.Second)

	b, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, content, string(b))
}

// xmlIndentationExceeds estimates the whitespace an indent would add and trips
// its budget without allocating it: a wide-and-deep tree is over, a small one
// is under, and a nil tree is trivially under.
func TestXMLIndentationEstimate(t *testing.T) {
	small := etree.NewDocument()
	root := small.CreateElement("server")
	root.CreateElement("motd").SetText("hello")
	require.False(t, xmlIndentationExceeds(small.Root(), 2, maxConfigFileSize))

	require.False(t, xmlIndentationExceeds(nil, 2, maxConfigFileSize))

	big := etree.NewDocument()
	parent := big.CreateElement("root")
	for i := 0; i < 300; i++ {
		parent = parent.CreateElement("a")
	}
	for i := 0; i < 200000; i++ {
		parent.CreateElement("x")
	}
	require.True(t, xmlIndentationExceeds(big.Root(), 2, maxConfigFileSize))

	// Comments are indented too, so a deep chain full of them must also be over
	// budget even though it contains no deep child elements.
	comments := etree.NewDocument()
	cparent := comments.CreateElement("root")
	for i := 0; i < 300; i++ {
		cparent = cparent.CreateElement("a")
	}
	for i := 0; i < 200000; i++ {
		cparent.CreateComment("x")
	}
	require.True(t, xmlIndentationExceeds(comments.Root(), 2, maxConfigFileSize))
}

// Indentation whitespace from non-element tokens counts too: a small file whose
// deep element chain holds a huge number of comments is refused before Indent
// allocates that whitespace, and left unchanged.
func TestXMLWithManyCommentsIsNotRewritten(t *testing.T) {
	const (
		depth    = 300
		comments = 200000
	)
	content := strings.Repeat("<a>", depth) + strings.Repeat("<!--x-->", comments) + strings.Repeat("</a>", depth)
	require.Less(t, len(content), maxConfigFileSize, "the crafted input must itself be under the size limit")

	path := filepath.Join(t.TempDir(), "config.xml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	var cf ConfigurationFile
	require.NoError(t, json.Unmarshal([]byte(`{"file":"config.xml","parser":"xml","replace":[{"match":"a","replace_with":"value"}]}`), &cf))
	cf.configuration = []byte(`{}`)

	f, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(t, err)
	defer f.Close()

	start := time.Now()
	err = cf.parseXmlFile(f)
	require.Error(t, err)
	require.Contains(t, err.Error(), "indented result exceeds limit")
	require.Less(t, time.Since(start), 5*time.Second)

	b, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, content, string(b))
}

// A document with more than one root element is refused before the depth and
// indentation guards (which only see the first root) and before Indent runs, so
// a second oversized tree cannot be indented past them. The file is unchanged.
func TestMultiRootXMLIsNotRewritten(t *testing.T) {
	const (
		depth    = 300
		children = 200000
	)
	bigTree := strings.Repeat("<a>", depth) + strings.Repeat("<x/>", children) + strings.Repeat("</a>", depth)
	content := "<safe/>" + bigTree

	path := filepath.Join(t.TempDir(), "config.xml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	var cf ConfigurationFile
	require.NoError(t, json.Unmarshal([]byte(`{"file":"config.xml","parser":"xml","replace":[{"match":"safe","replace_with":"value"}]}`), &cf))
	cf.configuration = []byte(`{}`)

	f, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(t, err)
	defer f.Close()

	start := time.Now()
	err = cf.parseXmlFile(f)
	require.Error(t, err)
	// Rejected by the multi-root guard, before any indentation work.
	require.Contains(t, err.Error(), "root elements")
	require.Less(t, time.Since(start), 5*time.Second)

	b, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, content, string(b))
}

// quotaRejectingFile is a configuration file whose Write is refused before any
// bytes are written, as a quota-tracked handle does when the result would not
// fit in the server's disk limit.
type quotaRejectingFile struct {
	*os.File
}

func (quotaRejectingFile) Write([]byte) (int, error) {
	return 0, os.ErrPermission
}

// A rewrite that would not fit within the disk limit leaves the file unchanged,
// rather than truncating it and then failing part way through writing it.
func TestRewriteThatDoesNotFitLeavesFileUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.properties")
	require.NoError(t, os.WriteFile(path, []byte("motd=hello\n"), 0o644))

	f, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(t, err)
	defer f.Close()

	require.Error(t, rewrite(quotaRejectingFile{f}, []byte("motd=a much longer message of the day\n")))

	b, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "motd=hello\n", string(b))
}
