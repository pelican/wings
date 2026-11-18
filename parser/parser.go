package parser

import (
	"bufio"
	"bytes"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"emperror.dev/errors"
	"github.com/apex/log"
	"github.com/beevik/etree"
	"github.com/goccy/go-json"
	"github.com/icza/dyno"
	"github.com/magiconair/properties"
	"github.com/pelletier/go-toml/v2"
	"github.com/tidwall/pretty"
	"gopkg.in/ini.v1"

	"github.com/pelican/wings/config"
)

// ConfigFile is the handle to a configuration file that is parsed and then rewritten
// in place. It is satisfied by both *os.File and filesystem.File.
type ConfigFile interface {
	io.Reader
	io.Writer
	io.Seeker
	Name() string
	Stat() (os.FileInfo, error)
	Truncate(size int64) error
}

// The file parsing options that are available for a server configuration file.
const (
	File       = "file"
	Yaml       = "yaml"
	Properties = "properties"
	Ini        = "ini"
	Json       = "json"
	Xml        = "xml"
	Toml       = "toml"
)

// maxTextScanTokenSize bounds how large a single line the "file" parser will buffer.
const maxTextScanTokenSize = 64 * 1024 * 1024

// maxConfigFileSize caps how large a configuration file we'll attempt to parse.
const maxConfigFileSize = 64 * 1024 * 1024

// maxXMLDepth is the deepest element nesting of an XML configuration file that
// is rewritten. Real configuration files are far shallower than this.
const maxXMLDepth = 512

// xmlDepth returns the element nesting depth of the tree rooted at root,
// without recursing, so that it cannot itself exhaust the stack.
func xmlDepth(root *etree.Element) int {
	if root == nil {
		return 0
	}
	type frame struct {
		e *etree.Element
		d int
	}
	stack := []frame{{root, 1}}
	depth := 0
	for len(stack) > 0 {
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if f.d > depth {
			depth = f.d
		}
		for _, c := range f.e.ChildElements() {
			stack = append(stack, frame{c, f.d + 1})
		}
	}
	return depth
}

// xmlIndentationExceeds reports whether indenting the tree rooted at root with
// the given number of spaces per level would add more than max bytes of
// whitespace. etree's Indent inserts, before every child element and before
// each parent's closing tag, a newline followed by depth*spaces spaces. On a
// crafted document (very many elements nested deeply) that indentation alone
// can be orders of magnitude larger than the input, exhausting memory inside
// Indent before the serialized output is ever size-checked. It walks the tree
// iteratively, like xmlDepth, and stops as soon as the budget is exceeded, so
// it never allocates the indentation it is estimating.
func xmlIndentationExceeds(root *etree.Element, spaces int, max int64) bool {
	if root == nil {
		return false
	}
	type frame struct {
		e *etree.Element
		d int
	}
	stack := []frame{{root, 0}}
	var total int64
	for len(stack) > 0 {
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		children := f.e.ChildElements()
		if len(children) > 0 {
			// One indent token before each child, plus one before the closing
			// tag; each is a newline and (d+1)*spaces spaces.
			token := int64(1 + (f.d+1)*spaces)
			total += token * int64(len(children)+1)
			if total > max {
				return true
			}
		}
		for _, c := range children {
			stack = append(stack, frame{c, f.d + 1})
		}
	}
	return total > max
}

// limitedWriter writes through to w until more than max bytes have been
// written, then fails with errConfigTooLarge rather than buffering an unbounded
// amount. It lets a document be serialized without first materializing a copy
// that could exhaust memory.
type limitedWriter struct {
	w   io.Writer
	n   int64
	max int64
}

var errConfigTooLarge = errors.Sentinel("parser: serialized configuration exceeds size limit")

func (lw *limitedWriter) Write(p []byte) (int, error) {
	if lw.n+int64(len(p)) > lw.max {
		return 0, errConfigTooLarge
	}
	n, err := lw.w.Write(p)
	lw.n += int64(n)
	return n, err
}

// ValueType is the JSON type of a replacement value as written in the egg.
type ValueType int

const (
	InvalidValue ValueType = iota
	StringValue
	NumberValue
	BooleanValue
	NullValue
	ObjectValue
	ArrayValue
)

// valueTypeOf maps a JSON value's kind to its ValueType.
func valueTypeOf(k jsontext.Kind) ValueType {
	switch k {
	case '"':
		return StringValue
	case '0':
		return NumberValue
	case 't', 'f':
		return BooleanValue
	case 'n':
		return NullValue
	case '{':
		return ObjectValue
	case '[':
		return ArrayValue
	default:
		return InvalidValue
	}
}

type ReplaceValue struct {
	value     []byte
	valueType ValueType
}

// newReplaceValue builds a ReplaceValue from a raw JSON value. Strings are
// unescaped up front, so an escaped section sign is stored as "§Foo"; every
// other type keeps its raw JSON bytes.
func newReplaceValue(v jsontext.Value) ReplaceValue {
	rv := ReplaceValue{valueType: valueTypeOf(v.Kind())}
	if rv.valueType == StringValue {
		// The decoder has already checked v's syntax, so the only error
		// AppendUnquote can still report is invalid UTF-8, which it has
		// already replaced with U+FFFD. (The old jsonparser path passed the
		// raw bytes through instead.)
		rv.value, _ = jsontext.AppendUnquote(nil, v)
	} else {
		rv.value = bytes.Clone(v)
	}
	return rv
}

// Value returns the underlying value of the replacement. Strings are already
// unescaped, so a section sign sent as a JSON escape comes back as "§Foo".
// Other types are returned as their raw JSON text.
func (cv *ReplaceValue) Value() []byte {
	return cv.value
}

// Type returns the underlying data type for the Value field.
func (cv *ReplaceValue) Type() ValueType {
	return cv.valueType
}

// String returns the value as a string representation.
func (cv *ReplaceValue) String() string {
	switch cv.Type() {
	case StringValue, BooleanValue, NumberValue:
		return string(cv.value)
	case NullValue:
		return "<nil>"
	default:
		return "<invalid>"
	}
}

func (cv *ReplaceValue) Bytes() []byte {
	switch cv.Type() {
	case StringValue, BooleanValue, NumberValue:
		return cv.value
	case NullValue:
		return []byte("<nil>")
	default:
		return []byte("<invalid>")
	}
}

type ConfigurationParser string

func (cp ConfigurationParser) String() string {
	return string(cp)
}

// ConfigurationFile defines a configuration file for the server startup. These
// will be looped over and modified before the server finishes booting.
type ConfigurationFile struct {
	FileName        string                         `json:"file"`
	Parser          ConfigurationParser            `json:"parser"`
	Replace         []ConfigurationFileReplacement `json:"replace"`
	AllowCreateFile bool                           `json:"create_file"` // assumed true by unmarshal as it was the original behaviour

	// Tracks Wings' configuration so that we can quickly get values
	// out of it when variables request it.
	configuration []byte
}

// UnmarshalJSON is a custom unmarshaler for configuration files. A replacement
// that fails to decode doesn't fail the entire operation: it is logged so
// someone can find the issue and skipped, and the other replacements are kept.
func (f *ConfigurationFile) UnmarshalJSON(data []byte) error {
	return jsonv2.Unmarshal(data, f, legacyDecodeOptions)
}

// legacyDecodeOptions keeps encoding/json v1 behaviour for egg JSON coming
// from the Panel: the last of any duplicate keys wins and invalid UTF-8 is
// accepted instead of failing the whole server configuration.
var legacyDecodeOptions = jsonv2.JoinOptions(
	jsontext.AllowDuplicateNames(true),
	jsontext.AllowInvalidUTF8(true),
)

// readObject reads a JSON object from dec in a single pass, calling fn with
// each member's name. fn must consume exactly one value from dec. A null
// object is treated as empty.
func readObject(dec *jsontext.Decoder, fn func(key string) error) error {
	tok, err := dec.ReadToken()
	if err != nil {
		return err
	}
	switch tok.Kind() {
	case 'n':
		return nil
	case '{':
	default:
		return errors.Errorf("parser: expected a JSON object, got %s", tok.Kind())
	}
	for dec.PeekKind() != '}' {
		tok, err := dec.ReadToken()
		if err != nil {
			return err
		}
		// tok is voided by the next decoder call, so the name is copied out
		// before fn reads the value.
		if err := fn(tok.String()); err != nil {
			return err
		}
	}
	_, err = dec.ReadToken()
	return err
}

// stringField holds the last value seen for a string member. A wrong type is
// recorded rather than returned straight away, so a later duplicate of the key
// can still replace it: the last occurrence wins, as with encoding/json v1.
type stringField struct {
	s   string
	set bool // false when the member was missing or null
	err error
}

// read consumes one value from dec into f, replacing anything read before.
func (f *stringField) read(dec *jsontext.Decoder, key string) error {
	*f = stringField{}
	switch k := dec.PeekKind(); k {
	case '"':
		tok, err := dec.ReadToken()
		if err != nil {
			return err
		}
		f.s, f.set = tok.String(), true
	case 'n':
		_, err := dec.ReadToken()
		return err
	default:
		f.err = errors.Errorf("parser: '%s' must be a string, got %s", key, k)
		return dec.SkipValue()
	}
	return nil
}

// UnmarshalJSONFrom decodes a configuration file in a single pass. A key whose
// value is null is treated as missing.
func (f *ConfigurationFile) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	var file, parser stringField
	var haveCreate bool
	var replace []ConfigurationFileReplacement
	var replaceErrs []error
	var createErr error
	f.AllowCreateFile = true
	err := readObject(dec, func(key string) error {
		switch key {
		case "file":
			return file.read(dec, key)
		case "parser":
			return parser.read(dec, key)
		case "replace":
			replace, replaceErrs = nil, nil
			switch k := dec.PeekKind(); k {
			case 'n':
				_, err := dec.ReadToken()
				return err
			case '[':
			default:
				replaceErrs = []error{errors.Errorf("parser: 'replace' must be an array, got %s", k)}
				return dec.SkipValue()
			}
			if _, err := dec.ReadToken(); err != nil {
				return err
			}
			// Each replacement is decoded from its own already-read value, so
			// a bad one is skipped and reported after the loop without
			// desyncing the decoder or dropping the good ones around it.
			replace = []ConfigurationFileReplacement{}
			for i := 0; dec.PeekKind() != ']'; i++ {
				val, err := dec.ReadValue()
				if err != nil {
					return err
				}
				if val.Kind() == 'n' {
					replaceErrs = append(replaceErrs, errors.Errorf("replace[%d]: replacement is null", i))
					continue
				}
				var r ConfigurationFileReplacement
				if err := jsonv2.Unmarshal(val, &r, dec.Options()); err != nil {
					replaceErrs = append(replaceErrs, errors.Wrapf(err, "replace[%d]", i))
					continue
				}
				replace = append(replace, r)
			}
			if _, err := dec.ReadToken(); err != nil {
				return err
			}
		case "create_file":
			val, err := dec.ReadValue()
			if err != nil {
				return err
			}
			haveCreate, createErr = true, nil
			switch val.Kind() {
			case 't':
				f.AllowCreateFile = true
			case 'f':
				f.AllowCreateFile = false
			case 'n':
				haveCreate, f.AllowCreateFile = false, true
			default:
				f.AllowCreateFile = true
				createErr = errors.Errorf("expected a boolean, got %s", val.Kind())
			}
		default:
			return dec.SkipValue()
		}
		return nil
	})
	if err != nil {
		return err
	}

	if file.err != nil {
		return file.err
	}
	if !file.set {
		return errors.New("parser: configuration file missing required 'file' key")
	}
	if parser.err != nil {
		return parser.err
	}
	if !parser.set {
		return errors.New("parser: configuration file missing required 'parser' key")
	}
	f.FileName, f.Parser = file.s, ConfigurationParser(parser.s)

	// Bad replacements are logged and skipped; the rest still apply.
	for _, err := range replaceErrs {
		log.WithField("file", f.FileName).WithField("error", err).Warn("failed to unmarshal configuration file replacement")
	}
	f.Replace = replace
	if f.Replace == nil {
		f.Replace = []ConfigurationFileReplacement{}
	}

	// create_file is assumed true when it is missing or invalid.
	if createErr != nil {
		log.WithField("file", f.FileName).WithField("error", createErr).Warn("create_file unmarshal failed")
	} else if !haveCreate {
		log.WithField("file", f.FileName).Debug("create_file not specified assumed true")
	}

	return nil
}

// ConfigurationFileReplacement defines a single find/replace instance for a
// given server configuration file.
type ConfigurationFileReplacement struct {
	Match       string       `json:"match"`
	IfValue     string       `json:"if_value"`
	ReplaceWith ReplaceValue `json:"replace_with"`
}

// UnmarshalJSON handles unmarshaling the JSON representation into a struct that
// provides more useful data to this functionality.
func (cfr *ConfigurationFileReplacement) UnmarshalJSON(data []byte) error {
	return jsonv2.Unmarshal(data, cfr, legacyDecodeOptions)
}

// UnmarshalJSONFrom decodes a single replacement in one pass. "match" and
// "replace_with" are required; "if_value" is optional. Eggs that predate
// "replace_with" use the old "value" key, which is accepted as a fallback.
func (cfr *ConfigurationFileReplacement) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	var match, ifValue stringField
	var haveReplace, haveLegacy bool
	var replaceWith, legacyValue ReplaceValue
	err := readObject(dec, func(key string) error {
		switch key {
		case "match":
			return match.read(dec, key)
		case "if_value":
			return ifValue.read(dec, key)
		case "replace_with", "value":
			val, err := dec.ReadValue()
			if err != nil {
				return err
			}
			if key == "replace_with" {
				replaceWith, haveReplace = newReplaceValue(val), true
			} else {
				legacyValue, haveLegacy = newReplaceValue(val), true
			}
		default:
			return dec.SkipValue()
		}
		return nil
	})
	if err != nil {
		return err
	}

	if match.err != nil {
		return match.err
	}
	if !match.set {
		return errors.New("parser: replacement missing required 'match' key")
	}
	// if_value is optional, and null means it is not set.
	if ifValue.err != nil {
		return ifValue.err
	}
	cfr.Match, cfr.IfValue = match.s, ifValue.s
	switch {
	case haveReplace:
		cfr.ReplaceWith = replaceWith
	case haveLegacy:
		cfr.ReplaceWith = legacyValue
	default:
		return errors.New("parser: replacement missing required 'replace_with' key")
	}

	return nil
}

type templatableConfig struct {
	Docker struct {
		Interface string `json:"interface"`
		Network   struct {
			Interface string `json:"interface"`
		} `json:"network"`
	} `json:"docker"`
}

func newTemplatableConfig(c *config.Configuration) templatableConfig {
	var t templatableConfig
	t.Docker.Interface = c.Docker.Network.Interface
	t.Docker.Network.Interface = c.Docker.Network.Interface
	return t
}

// Parse parses a given configuration file and updates all the values within
// as defined in the API response from the Panel.
func (f *ConfigurationFile) Parse(file ConfigFile) error {
	//log.WithField("path", path).WithField("parser", f.Parser.String()).Debug("parsing server configuration file")

	// Refuse to parse files larger than the cap. Every parser below buffers the
	// whole file in memory, and the contents are untrusted server-owned input, so
	// this guards the daemon against being OOM'd by an oversized config. The
	// server is still free to boot with the file as-is; we just don't rewrite it.
	if info, err := file.Stat(); err != nil {
		return err
	} else if info.Size() > maxConfigFileSize {
		return errors.Errorf("parser: refusing to parse configuration file %q: size %d exceeds limit of %d bytes", file.Name(), info.Size(), maxConfigFileSize)
	}

	// What the fuck is going on here?
	if mb, err := json.Marshal(newTemplatableConfig(config.Get())); err != nil {
		return err
	} else {
		f.configuration = mb
	}

	var err error

	switch f.Parser {
	case Properties:
		err = f.parsePropertiesFile(file)
	case File:
		err = f.parseTextFile(file)
	case Yaml, "yml":
		err = f.parseYamlFile(file)
	case Json:
		err = f.parseJsonFile(file)
	case Ini:
		err = f.parseIniFile(file)
	case Xml:
		err = f.parseXmlFile(file)
	case Toml:
		err = f.parseTomlFile(file)
	}
	return err
}

// Parses an xml file.
func (f *ConfigurationFile) parseXmlFile(file ConfigFile) error {
	doc := etree.NewDocument()
	if _, err := doc.ReadFrom(io.LimitReader(file, maxConfigFileSize)); err != nil {
		return err
	}
	// The file is controlled by the server, and a deeply nested document could
	// exhaust the stack while it is rewritten. The server still boots with the
	// file as it is, the same as for files over the size limit.
	if depth := xmlDepth(doc.Root()); depth > maxXMLDepth {
		return errors.Errorf("parser: refusing to parse configuration file %q: element nesting depth %d exceeds limit of %d", file.Name(), depth, maxXMLDepth)
	}

	// If there is no root we should create a basic start to the file. This isn't required though,
	// and if it doesn't work correctly I'll just remove the code.
	if doc.Root() == nil {
		doc.CreateProcInst("xml", `version="1.0" encoding="utf-8"`)
	}

	for i, replacement := range f.Replace {
		value, err := f.LookupConfigurationValue(replacement)
		if err != nil {
			return err
		}

		// If this is the first item and there is no root element, create that root now and apply
		// it for future use.
		if i == 0 && doc.Root() == nil {
			parts := strings.SplitN(replacement.Match, ".", 2)
			doc.SetRoot(doc.CreateElement(parts[0]))
		}

		path := "./" + strings.Replace(replacement.Match, ".", "/", -1)

		// If we're not doing a wildcard replacement go ahead and create the
		// missing element if we cannot find it yet.
		if !strings.Contains(path, "*") {
			parts := strings.Split(replacement.Match, ".")

			// Set the initial element to be the root element, and then work from there.
			element := doc.Root()

			// Iterate over the path to create the required structure for the given element's path.
			// This does not set a value, only ensures that the base structure exists. We start at index
			// 1 because an XML document can only contain a single root element, and from there we'll
			// work our way down the chain.
			for _, tag := range parts[1:] {
				if e := element.FindElement(tag); e == nil {
					element = element.CreateElement(tag)
				} else {
					element = e
				}
			}
		}

		// Iterate over the elements we found and update their values.
		for _, element := range doc.FindElements(path) {
			if xmlValueMatchRegex.MatchString(value) {
				k := xmlValueMatchRegex.ReplaceAllString(value, "$1")
				v := xmlValueMatchRegex.ReplaceAllString(value, "$2")

				element.RemoveAttr(k)
				element.CreateAttr(k, v)
			} else {
				element.SetText(value)
			}
		}
	}

	// Indenting the document inserts whitespace text nodes into the tree. On a
	// crafted file with very many deeply nested elements that whitespace alone
	// can be orders of magnitude larger than the input and exhaust memory, so
	// refuse the rewrite before Indent allocates it. The server still boots with
	// the file as it is, the same as for files over the size limit.
	if xmlIndentationExceeds(doc.Root(), 2, maxConfigFileSize) {
		return errors.Errorf("parser: refusing to write configuration file %q: indented result exceeds limit of %d bytes", file.Name(), maxConfigFileSize)
	}

	// Ensure the XML is indented properly.
	doc.Indent(2)

	// Serialize through a writer bounded to the size limit so a document that
	// still expands past it cannot buffer an unbounded copy in memory.
	var buf bytes.Buffer
	if _, err := doc.WriteTo(&limitedWriter{w: &buf, max: maxConfigFileSize}); err != nil {
		if errors.Is(err, errConfigTooLarge) {
			return errors.Errorf("parser: refusing to write configuration file %q: result exceeds limit of %d bytes", file.Name(), maxConfigFileSize)
		}
		return err
	}
	return rewrite(file, buf.Bytes())
}

// Parses an ini file.
func (f *ConfigurationFile) parseIniFile(file ConfigFile) error {
	// Wrap the file in a NopCloser so the ini package doesn't close the file.
	cfg, err := ini.Load(io.NopCloser(io.LimitReader(file, maxConfigFileSize)))
	if err != nil {
		return err
	}

	// Force format to be key = value
	ini.PrettyFormat = false
	ini.PrettyEqual = true

	for _, replacement := range f.Replace {
		var (
			path         []string
			bracketDepth int
			v            []int32
		)
		for _, c := range replacement.Match {
			switch c {
			case '[':
				bracketDepth++
			case ']':
				bracketDepth--
			case '.':
				if bracketDepth > 0 || len(path) == 1 {
					v = append(v, c)
					continue
				}
				path = append(path, string(v))
				v = v[:0]
			default:
				v = append(v, c)
			}
		}
		path = append(path, string(v))

		value, err := f.LookupConfigurationValue(replacement)
		if err != nil {
			return err
		}

		k := path[0]
		s := cfg.Section("")
		// Passing a key of foo.bar will look for "bar" in the "[foo]" section of the file.
		if len(path) == 2 {
			k = path[1]
			s = cfg.Section(path[0])
		}

		// If no section was found, create that new section now and then set the
		// section value we're using to be the new one.
		if s == nil {
			s, err = cfg.NewSection(path[0])
			if err != nil {
				return err
			}
		}

		// If the key exists in the file go ahead and set the value, otherwise try to
		// create it in the section.
		if s.HasKey(k) {
			s.Key(k).SetValue(value)
		} else {
			if _, err := s.NewKey(k, value); err != nil {
				return err
			}
		}
	}

	var buf bytes.Buffer
	if _, err := cfg.WriteTo(&buf); err != nil {
		return err
	}
	return rewrite(file, buf.Bytes())
}

// Parses a json file updating any matching key/value pairs. If a match is not found, the
// value is set regardless in the file. See the commentary in parseYamlFile for more details
// about what is happening during this process.
func (f *ConfigurationFile) parseJsonFile(file ConfigFile) error {
	b, err := io.ReadAll(io.LimitReader(file, maxConfigFileSize))
	if err != nil {
		return err
	}

	data, err := f.IterateOverJson(b)
	if err != nil {
		return err
	}

	prettified := pretty.PrettyOptions(data, &pretty.Options{
		Width:    80,
		Prefix:   "",
		Indent:   "  ",
		SortKeys: false,
	})

	if err := rewrite(file, prettified); err != nil {
		return err
	}
	return nil
}

// Parses a yaml file and updates any matching key/value pairs before persisting
// it back to the disk.
func (f *ConfigurationFile) parseYamlFile(file ConfigFile) error {
	b, err := io.ReadAll(io.LimitReader(file, maxConfigFileSize))
	if err != nil {
		return err
	}

	// Apply the replacements while preserving the layout, comments and key order
	// of the original file. See updateYaml for details.
	marshaled, changed, err := f.updateYaml(b)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}

	return rewrite(file, marshaled)
}

// Parses a toml file and updates any matching key/value pairs before persisting
// it back to the disk.
func (f *ConfigurationFile) parseTomlFile(file ConfigFile) error {
	b, err := io.ReadAll(io.LimitReader(file, maxConfigFileSize))
	if err != nil {
		return err
	}

	i := make(map[string]interface{})
	if err := toml.Unmarshal(b, &i); err != nil {
		return err
	}

	// Convert to JSON to reuse IterateOverJson for value replacement.
	jsonBytes, err := json.Marshal(dyno.ConvertMapI2MapS(i))
	if err != nil {
		return err
	}

	data, err := f.IterateOverJson(jsonBytes)
	if err != nil {
		return err
	}

	var jsonData interface{}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&jsonData); err != nil {
		return err
	}
	jsonData = normalizeTomlTypes(jsonData)

	// Remarshal back to TOML format.
	marshaled, err := toml.Marshal(jsonData)
	if err != nil {
		return err
	}

	return rewrite(file, marshaled)
}

// normalizeYamlTypes converts json.Number values (produced by UseNumber()) into
// proper Go numeric types so that yaml.Marshal writes integers as plain integers
// instead of float64 scientific notation (e.g. 1.5e+18 for large Snowflake IDs).
func normalizeYamlTypes(value interface{}) interface{} {
	switch typed := value.(type) {
	case map[string]interface{}:
		for key, item := range typed {
			typed[key] = normalizeYamlTypes(item)
		}
		return typed
	case []interface{}:
		for i := range typed {
			typed[i] = normalizeYamlTypes(typed[i])
		}
		return typed
	case json.Number:
		s := typed.String()
		// Preserve float representation: if the number contains '.', 'e' or 'E'
		// it was originally a float and must not be coerced to int64.
		if strings.ContainsAny(s, ".eE") {
			if floatVal, err := typed.Float64(); err == nil {
				return floatVal
			}
			return s
		}
		if intVal, err := typed.Int64(); err == nil {
			return intVal
		}
		if floatVal, err := typed.Float64(); err == nil {
			return floatVal
		}
		return s
	default:
		return value
	}
}

func normalizeTomlTypes(value interface{}) interface{} {
	switch typed := value.(type) {
	case map[string]interface{}:
		for key, item := range typed {
			typed[key] = normalizeTomlTypes(item)
		}
		return typed
	case []interface{}:
		for i := range typed {
			typed[i] = normalizeTomlTypes(typed[i])
		}
		return typed
	case json.Number:
		if intVal, err := typed.Int64(); err == nil {
			return intVal
		}
		if floatVal, err := typed.Float64(); err == nil {
			return floatVal
		}
		return typed.String()
	case string:
		if timeVal, err := time.Parse(time.RFC3339Nano, typed); err == nil {
			return timeVal
		}
		if timeVal, err := time.Parse(time.RFC3339, typed); err == nil {
			return timeVal
		}
		return typed
	default:
		return value
	}
}

// Parses a text file using basic find and replace. This is a highly inefficient method of
// scanning a file and performing a replacement. You should attempt to use anything other
// than this function where possible.
func (f *ConfigurationFile) parseTextFile(file ConfigFile) error {
	b := bytes.NewBuffer(nil)
	s := bufio.NewScanner(io.LimitReader(file, maxConfigFileSize))
	s.Buffer(make([]byte, 0, 64*1024), maxTextScanTokenSize)
	var replaced bool
	for s.Scan() {
		line := s.Bytes()
		replaced = false
		for _, replace := range f.Replace {
			// If this line doesn't match what we expect for the replacement, move on to the next
			// line. Otherwise, update the line to have the replacement value.
			if !bytes.HasPrefix(line, []byte(replace.Match)) {
				continue
			}
			b.Write(replace.ReplaceWith.Bytes())
			replaced = true
		}
		if !replaced {
			b.Write(line)
		}
		b.WriteByte('\n')
		// Replacements can make the result much larger than the file that was
		// read, so apply the same limit while building it.
		if b.Len() > maxConfigFileSize {
			return errors.Errorf("parser: refusing to write configuration file %q: result exceeds limit of %d bytes", file.Name(), maxConfigFileSize)
		}
	}
	if err := s.Err(); err != nil {
		return errors.Wrap(err, "parser: failed to scan text file for configuration update")
	}

	return rewrite(file, b.Bytes())
}

// parsePropertiesFile parses a properties file and updates the values within it
// to match those that are passed. Once completed the new file is written to the
// disk. This will cause comments not present at the head of the file to be
// removed unfortunately.
//
// Any UTF-8 value will be written back to the disk as their escaped value rather
// than the raw value There is no winning with this logic. This fixes a bug where
// users with hand rolled UTF-8 escape sequences would have all sorts of pain in
// their configurations because we were writing the UTF-8 literal characters which
// their games could not actually handle.
//
// However, by adding this fix to only store the escaped UTF-8 sequence we
// unwittingly introduced a "regression" that causes _other_ games to have issues
// because they can only handle the unescaped representations. I cannot think of
// a simple approach to this problem that doesn't just lead to more complicated
// cases and problems.
//
// So, if your game cannot handle parsing UTF-8 sequences that are escaped into
// the string, well, sucks. There are fewer of those games than there are games
// that have issues parsing the raw UTF-8 sequence into a string? Also how does
// one really know what the user intended at this point? We'd need to know if
// the value was escaped or not to begin with before setting it, which I suppose
// can work but jesus that is going to be some annoyingly complicated logic?
//
// @see https://github.com/pterodactyl/panel/issues/2308 (original)
// @see https://github.com/pterodactyl/panel/issues/3009 ("bug" introduced as result)
func (f *ConfigurationFile) parsePropertiesFile(file ConfigFile) error {
	b, err := io.ReadAll(io.LimitReader(file, maxConfigFileSize))
	if err != nil {
		return err
	}

	s := bytes.NewBuffer(nil)
	scanner := bufio.NewScanner(bytes.NewReader(b))
	// Scan until we hit a line that is not a comment that actually has content
	// on it. Keep appending the comments until that time.
	for scanner.Scan() {
		text := scanner.Bytes()
		if len(text) > 0 && text[0] != '#' {
			break
		}
		s.Write(text)
		s.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return errors.WithStackIf(err)
	}

	p, err := properties.Load(b, properties.UTF8)
	if err != nil {
		return errors.Wrap(err, "parser: could not load properties file for configuration update")
	}

	// Replace any values that need to be replaced.
	for _, replace := range f.Replace {
		data, err := f.LookupConfigurationValue(replace)
		if err != nil {
			return errors.Wrap(err, "parser: failed to lookup configuration value")
		}

		v, ok := p.Get(replace.Match)
		// Don't attempt to replace the value if we're looking for a specific value and
		// it does not match. If there was no match at all in the file for this key but
		// we're doing an IfValue match, do nothing.
		if replace.IfValue != "" && (!ok || (ok && v != replace.IfValue)) {
			continue
		}

		if _, _, err := p.Set(replace.Match, data); err != nil {
			return errors.Wrap(err, "parser: failed to set replacement value")
		}
	}

	// Add the new file content to the string builder.
	for _, key := range p.Keys() {
		value, ok := p.Get(key)
		if !ok {
			continue
		}
		// This escape is intentional!
		//
		// See the docblock for this function for more details, do not change this
		// or you'll cause a flood of new issue reports no one wants to deal with.
		s.WriteString(key + "=" + strings.Trim(strconv.QuoteToASCII(value), "\"") + "\n")
	}

	return rewrite(file, s.Bytes())
}

// rewrite replaces the contents of file with data. The new contents are written
// first and the old tail is trimmed afterwards, rather than truncating first, so
// that a write the quota-tracked handle refuses (because the result would not fit
// in the server's disk limit) leaves the original file intact instead of emptying
// it. A shrinking replacement never needs to reserve space, so it cannot fail
// that way either.
func rewrite(file ConfigFile, data []byte) error {
	if len(data) > maxConfigFileSize {
		return errors.Errorf("parser: refusing to write configuration file %q: result exceeds limit of %d bytes", file.Name(), maxConfigFileSize)
	}

	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	n, err := file.Write(data)
	if err != nil {
		return errors.Wrap(err, "parser: failed to write configuration file to disk")
	}
	// Remove any bytes of the old, longer contents left after the new ones.
	if err := file.Truncate(int64(n)); err != nil {
		return errors.Wrap(err, "parser: failed to trim configuration file on disk")
	}
	return nil
}
