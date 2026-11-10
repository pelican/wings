package parser

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"emperror.dev/errors"
	"github.com/apex/log"
	"github.com/beevik/etree"
	"github.com/buger/jsonparser"
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

type ReplaceValue struct {
	value     []byte
	valueType jsonparser.ValueType
}

// Value returns the underlying value of the replacement. Be aware that this
// can include escaped UTF-8 sequences that will need to be handled by the caller
// in order to avoid accidentally injecting invalid sequences into the running
// process.
//
// For example the expected value may be "§Foo" but you'll be working directly
// with "\u00a7FOo" for this value. This will cause user pain if not solved since
// that is clearly not the value they were expecting to be using.
func (cv *ReplaceValue) Value() []byte {
	return cv.value
}

// Type returns the underlying data type for the Value field.
func (cv *ReplaceValue) Type() jsonparser.ValueType {
	return cv.valueType
}

// String returns the value as a string representation. This will automatically
// handle casting the UTF-8 sequence into the expected value, switching something
// like "\u00a7Foo" into "§Foo".
func (cv *ReplaceValue) String() string {
	switch cv.Type() {
	case jsonparser.String:
		str, err := jsonparser.ParseString(cv.value)
		if err != nil {
			panic(errors.Wrap(err, "parser: could not parse value"))
		}
		return str
	case jsonparser.Null:
		return "<nil>"
	case jsonparser.Boolean:
		return string(cv.value)
	case jsonparser.Number:
		return string(cv.value)
	default:
		return "<invalid>"
	}
}

func (cv *ReplaceValue) Bytes() []byte {
	switch cv.Type() {
	case jsonparser.String:
		var stackbuf [64]byte
		bU, err := jsonparser.Unescape(cv.value, stackbuf[:])
		if err != nil {
			panic(errors.Wrap(err, "parser: could not parse value"))
		}
		return bU
	case jsonparser.Null:
		return []byte("<nil>")
	case jsonparser.Boolean:
		return cv.value
	case jsonparser.Number:
		return cv.value
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

// UnmarshalJSON is a custom unmarshaler for configuration files. If there is an
// error while parsing out the replacements, don't fail the entire operation,
// just log a global warning so someone can find the issue, and return an empty
// array of replacements.
func (f *ConfigurationFile) UnmarshalJSON(data []byte) error {
	var m map[string]*json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}

	fileRaw, ok := m["file"]
	if !ok || fileRaw == nil {
		return errors.New("parser: configuration file missing required 'file' key")
	}
	if err := json.Unmarshal(*fileRaw, &f.FileName); err != nil {
		return err
	}

	parserRaw, ok := m["parser"]
	if !ok || parserRaw == nil {
		return errors.New("parser: configuration file missing required 'parser' key")
	}
	if err := json.Unmarshal(*parserRaw, &f.Parser); err != nil {
		return err
	}

	f.Replace = []ConfigurationFileReplacement{}
	if replaceRaw, ok := m["replace"]; ok && replaceRaw != nil {
		if err := json.Unmarshal(*replaceRaw, &f.Replace); err != nil {
			log.WithField("file", f.FileName).WithField("error", err).Warn("failed to unmarshal configuration file replacement")
		}
	}

	// test if "create_file" exists, if not just assume true
	if val, exists := m["create_file"]; exists && val != nil {
		if err := json.Unmarshal(*val, &f.AllowCreateFile); err != nil {
			log.WithField("file", f.FileName).WithField("error", err).Warn("create_file unmarshal failed")
			f.AllowCreateFile = true
		}
	} else {
		log.WithField("file", f.FileName).Debug("create_file not specified assumed true")
		f.AllowCreateFile = true
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
	m, err := jsonparser.GetString(data, "match")
	if err != nil {
		return err
	}

	cfr.Match = m

	iv, err := jsonparser.GetString(data, "if_value")
	// We only check keypath here since match & replace_with should be present on all of
	// them, however if_value is optional.
	if err != nil && !errors.Is(err, jsonparser.KeyPathNotFoundError) {
		return err
	}
	cfr.IfValue = iv

	rw, dt, _, err := jsonparser.Get(data, "replace_with")
	if err != nil {
		if !errors.Is(err, jsonparser.KeyPathNotFoundError) {
			return err
		}

		// Okay, likely dealing with someone who forgot to upgrade their eggs, so in
		// that case, fallback to using the old key which was "value".
		rw, dt, _, err = jsonparser.Get(data, "value")
		if err != nil {
			return err
		}
	}

	cfr.ReplaceWith = ReplaceValue{
		value:     rw,
		valueType: dt,
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

	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := file.Truncate(0); err != nil {
		return err
	}

	// Ensure the XML is indented properly.
	doc.Indent(2)

	// Write the XML to the file.
	if _, err := doc.WriteTo(file); err != nil {
		return err
	}
	return nil
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

	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := file.Truncate(0); err != nil {
		return err
	}

	if _, err := cfg.WriteTo(file); err != nil {
		return err
	}
	return nil
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

	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := file.Truncate(0); err != nil {
		return err
	}

	prettified := pretty.PrettyOptions(data, &pretty.Options{
		Width:    80,
		Prefix:   "",
		Indent:   "  ",
		SortKeys: false,
	})

	if _, err := io.Copy(file, bytes.NewReader(prettified)); err != nil {
		return errors.Wrap(err, "parser: failed to write properties file to disk")
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

	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := file.Truncate(0); err != nil {
		return err
	}

	// Write the data to the file.
	if _, err := io.Copy(file, bytes.NewReader(marshaled)); err != nil {
		return errors.Wrap(err, "parser: failed to write properties file to disk")
	}
	return nil
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

	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := file.Truncate(0); err != nil {
		return err
	}

	if _, err := io.Copy(file, bytes.NewReader(marshaled)); err != nil {
		return errors.Wrap(err, "parser: failed to write toml file to disk")
	}
	return nil
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
	}
	if err := s.Err(); err != nil {
		return errors.Wrap(err, "parser: failed to scan text file for configuration update")
	}

	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := file.Truncate(0); err != nil {
		return err
	}

	// Write the data to the file.
	if _, err := io.Copy(file, b); err != nil {
		return errors.Wrap(err, "parser: failed to write properties file to disk")
	}
	return nil
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

	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := file.Truncate(0); err != nil {
		return err
	}

	// Write the data to the file.
	if _, err := io.Copy(file, s); err != nil {
		return errors.Wrap(err, "parser: failed to write properties file to disk")
	}
	return nil
}
