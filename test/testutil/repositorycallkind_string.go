// Copyright 2018 GRAIL, Inc. All rights reserved.
// Use of this source code is governed by the Apache 2.0
// license that can be found in the LICENSE file.

// Code generated by "stringer -type=RepositoryCallKind"; DO NOT EDIT

package testutil

import "fmt"

const _RepositoryCallKind_name = "RepositoryGetRepositoryPutRepositoryWriteToRepositoryReadFrom"

var _RepositoryCallKind_index = [...]uint8{0, 13, 26, 43, 61}

func (i RepositoryCallKind) String() string {
	if i < 0 || i >= RepositoryCallKind(len(_RepositoryCallKind_index)-1) {
		return fmt.Sprintf("RepositoryCallKind(%d)", i)
	}
	return _RepositoryCallKind_name[_RepositoryCallKind_index[i]:_RepositoryCallKind_index[i+1]]
}