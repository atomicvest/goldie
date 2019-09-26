// Package goldie provides test assertions based on golden files. It's typically
// used for testing responses with larger data bodies.
//
// The concept is straight forward. Valid response data is stored in a "golden
// file". The actual response data will be byte compared with the golden file
// and the test will fail if there is a difference.
//
// Updating the golden file can be done by running `go test -update ./...`.
package goldie

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/template"

	"errors"

	"github.com/pmezard/go-difflib/difflib"
	"github.com/sergi/go-diff/diffmatchpatch"
)

// Compile time assurance
var _ Tester = &goldie{}
var _ OptionProcessor = &goldie{}

type goldie struct {
	fixtureDir     string
	fileNameSuffix string
	filePerms      os.FileMode
	dirPerms       os.FileMode

	diffProcessor        DiffProcessor
	diffFn               DiffFn
	ignoreTemplateErrors bool
	useTestNameForDir    bool
	useSubTestNameForDir bool
}

// === OptionProcessor ===============================

func (g *goldie) WithFixtureDir(dir string) error {
	g.fixtureDir = dir
	return nil
}

func (g *goldie) WithNameSuffix(suffix string) error {
	g.fileNameSuffix = suffix
	return nil
}

func (g *goldie) WithFilePerms(mode os.FileMode) error {
	g.filePerms = mode
	return nil
}

func (g *goldie) WithDirPerms(mode os.FileMode) error {
	g.dirPerms = mode
	return nil
}

func (g *goldie) WithDiffEngine(engine DiffProcessor) error {
	g.diffProcessor = engine
	return nil
}

func (g *goldie) WithDiffFn(fn DiffFn) error {
	g.diffFn = fn
	return nil
}

func (g *goldie) WithIgnoreTemplateErrors(ignoreErrors bool) error {
	g.ignoreTemplateErrors = ignoreErrors
	return nil
}

func (g *goldie) WithTestNameForDir(use bool) error {
	g.useTestNameForDir = use
	return nil
}

func (g *goldie) WithSubTestNameForDir(use bool) error {
	g.useSubTestNameForDir = use
	return nil
}

// Assert compares the actual data received with the expected data in the
// golden files. If the update flag is set, it will also update the golden
// file.
//
// `name` refers to the name of the test and it should typically be unique
// within the package. Also it should be a valid file name (so keeping to
// `a-z0-9\-\_` is a good idea).
func (g *goldie) Assert(t *testing.T, name string, actualData []byte) {
	if *update {
		err := g.Update(t, name, actualData)
		if err != nil {
			t.Error(err)
			t.FailNow()
		}
	}

	err := g.compare(t, name, actualData)
	if err != nil {
		{
			var e *errFixtureNotFound
			if errors.As(err, &e) {
				t.Error(err)
				t.FailNow()
				return
			}
		}

		{
			var e *errFixtureMismatch
			if errors.As(err, &e) {
				t.Error(err)
				return
			}
		}

		t.Error(err)
	}
}

// AssertJson compares the actual json data received with expected data in the
// golden files. If the update flag is set, it will also update the golden
// file.
//
// `name` refers to the name of the test and it should typically be unique
// within the package. Also it should be a valid file name (so keeping to
// `a-z0-9\-\_` is a good idea).
func (g *goldie) AssertJson(t *testing.T, name string, actualJsonData interface{}) {
	js, err := json.MarshalIndent(actualJsonData, "", "  ")

	if err != nil {
		t.Error(err)
		t.FailNow()
	}

	g.Assert(t, name, normalizeLF(js))
}

// normalizeLF normalizes line feed character set across os (es)
// \r\n (windows) & \r (mac) into \n (unix)
func normalizeLF(d []byte) []byte {
	// if empty / nil return as is
	if len(d) == 0 {
		return d
	}
	// replace CR LF \r\n (windows) with LF \n (unix)
	d = bytes.Replace(d, []byte{13, 10}, []byte{10}, -1)
	// replace CF \r (mac) with LF \n (unix)
	d = bytes.Replace(d, []byte{13}, []byte{10}, -1)
	return d
}

// Assert compares the actual data received with the expected data in the
// golden files after executing it as a template with data parameter.
// If the update flag is set, it will also update the golden file.
// `name` refers to the name of the test and it should typically be unique
// within the package. Also it should be a valid file name (so keeping to
// `a-z0-9\-\_` is a good idea).
func (g *goldie) AssertWithTemplate(t *testing.T, name string, data interface{}, actualData []byte) {
	if *update {
		err := g.Update(t, name, actualData)
		if err != nil {
			t.Error(err)
			t.FailNow()
		}
	}

	err := g.compareTemplate(t, name, data, actualData)
	if err != nil {
		{
			var e *errFixtureNotFound
			if errors.As(err, &e) {
				t.Error(err)
				t.FailNow()
				return
			}
		}

		{
			var e *errFixtureMismatch
			if errors.As(err, &e) {
				t.Error(err)
				return
			}
		}

		t.Error(err)
	}
}

// Update will update the golden fixtures with the received actual data.
//
// This method does not need to be called from code, but it's exposed so that it
// can be explicitly called if needed. The more common approach would be to
// update using `go test -update ./...`.
func (g *goldie) Update(t *testing.T, name string, actualData []byte) error {
	if err := g.ensureDir(filepath.Dir(g.goldenFileName(t, name))); err != nil {
		return err
	}

	return ioutil.WriteFile(g.goldenFileName(t, name), actualData, g.filePerms)
}

// compare is reading the golden fixture file and compare the stored data with
// the actual data.
func (g *goldie) compare(t *testing.T, name string, actualData []byte) error {
	expectedData, err := ioutil.ReadFile(g.goldenFileName(t, name))

	if err != nil {
		if os.IsNotExist(err) {
			return newErrFixtureNotFound()
		}

		return fmt.Errorf("Expected %s to be nil", err.Error())
	}

	if !bytes.Equal(actualData, expectedData) {
		msg := "Result did not match the golden fixture.\n"
		actual := string(actualData)
		expected := string(expectedData)

		if g.diffFn != nil || g.diffProcessor != UndefinedDiff {
			var d string
			if g.diffFn != nil {
				d = g.diffFn(actual, expected)
			} else {
				d = diff(g.diffProcessor, actual, expected)
			}

			msg += "Diff is below:\n" + d
		} else {
			msg = fmt.Sprintf("%sExpected: %s\n"+
				"Got: %s",
				msg,
				expected,
				actual)
		}
		return newErrFixtureMismatch(msg)
	}

	return nil
}

func diff(engine DiffProcessor, actual string, expected string) string {
	var diff string
	switch engine {
	case ClassicDiff:
		diff, _ = difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
			A:        difflib.SplitLines(expected),
			B:        difflib.SplitLines(actual),
			FromFile: "Expected",
			FromDate: "",
			ToFile:   "Actual",
			ToDate:   "",
			Context:  1,
		})

	case ColoredDiff:
		dmp := diffmatchpatch.New()
		diffs := dmp.DiffMain(actual, expected, false)
		diff = dmp.DiffPrettyText(diffs)
	}
	return diff
}

// compareTemplate is reading the golden fixture file and compare the stored
// data with the actual data.
func (g *goldie) compareTemplate(t *testing.T, name string, data interface{}, actualData []byte) error {
	expectedDataTmpl, err := ioutil.ReadFile(g.goldenFileName(t, name))

	if err != nil {
		if os.IsNotExist(err) {
			return newErrFixtureNotFound()
		}

		return fmt.Errorf("Expected %s to be nil", err.Error())
	}

	missingKey := "error"
	if g.ignoreTemplateErrors {
		missingKey = "default"
	}
	tmpl, err := template.New("test").Option("missingkey=" + missingKey).Parse(string(expectedDataTmpl))
	if err != nil {
		return fmt.Errorf("Expected %s to be nil", err.Error())
	}

	var expectedData bytes.Buffer
	err = tmpl.Execute(&expectedData, data)
	if err != nil {
		return newErrMissingKey(fmt.Sprintf("Template error: %s", err.Error()))
	}

	if !bytes.Equal(actualData, expectedData.Bytes()) {
		return newErrFixtureMismatch(
			fmt.Sprintf("Result did not match the golden fixture.\n"+
				"Expected: %s\n"+
				"Got: %s",
				string(expectedData.Bytes()),
				string(actualData)))
	}

	return nil
}

// ensureDir will create the fixture folder if it does not already exist.
func (g *goldie) ensureDir(loc string) error {
	s, err := os.Stat(loc)
	switch {
	case err != nil && os.IsNotExist(err):
		// the location does not exist, so make directories to there
		return os.MkdirAll(loc, g.dirPerms)
	case err == nil && !s.IsDir():
		return newErrFixtureDirectoryIsFile(loc)
	}

	return err
}

// goldenFileName simply returns the file name of the golden file fixture.
func (g *goldie) goldenFileName(t *testing.T, name string) string {

	dir := g.fixtureDir

	if g.useTestNameForDir {
		dir = filepath.Join(dir, strings.Split(t.Name(), "/")[0])
	}

	if g.useSubTestNameForDir {
		n := strings.Split(t.Name(), "/")
		if len(n) > 1 {

			dir = filepath.Join(dir, n[1])
		}
	}

	return filepath.Join(dir, fmt.Sprintf("%s%s", name, g.fileNameSuffix))
}
