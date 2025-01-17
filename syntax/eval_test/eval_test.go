// Copyright 2020 GRAIL, Inc. All rights reserved.
// Use of this source code is governed by the Apache 2.0
// license that can be found in the LICENSE file.

package eval_test

import (
	"strings"
	"testing"

	"github.com/grailbio/reflow/syntax"
	"github.com/grailbio/reflow/test/testutil"
)

func TestEval(t *testing.T) {
	tests := []string{
		"testdata/test1.rf",
		"testdata/arith.rf",
		"testdata/prec.rf",
		"testdata/missingnewline.rf",
		"testdata/strings.rf",
		"testdata/path.rf",
		"testdata/typealias.rf",
		"testdata/typealias2.rf",
		"testdata/newmodule.rf",
		"testdata/delayed.rf",
		"testdata/float.rf",
		"testdata/regexp.rf",
		"testdata/compare.rf",
		"testdata/if.rf",
		"testdata/dirs.rf",
		"testdata/switch.rf",
		"testdata/builtin_override.rf",
		"testdata/reduce.rf",
		"testdata/fold.rf",
		"testdata/test_flag_dependence.rf",
		"testdata/compr.rf",
		"testdata/files.rf",
	}
	testutil.RunReflowTests(t, tests)
}

func TestEvalErr(t *testing.T) {
	sess := syntax.NewSession(nil)
	for _, c := range []struct {
		file string
		err  string
	}{
		{"testdata/strings_err1.rf", "number has no digits"},
		{"testdata/strings_err2.rf", "number has no digits"},
		{"testdata/strings_err3.rf", "expected end of string, found '-'"},
		{"testdata/map_compr_err.rf", "failed assertion map_compr_err.TestMapComprErr"},
		{"testdata/list_compr_err.rf", "failed assertion list_compr_err.TestListComprErr"},
	} {
		m, err := sess.Open(c.file)
		if err != nil {
			t.Errorf("%s: %v", c.file, err)
			continue
		}
		_, err = m.Make(sess, sess.Values)
		if err == nil {
			t.Errorf("%s: expected error", c.file)
			continue
		}
		if got, want := err.Error(), c.err; !strings.Contains(got, want) {
			t.Errorf("%s: got '%v', want '%v'", c.file, got, want)
		}
	}
}
