// Copyright 2017 GRAIL, Inc. All rights reserved.
// Use of this source code is governed by the Apache 2.0
// license that can be found in the LICENSE file.

package s3walker

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/s3"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/grailbio/base/admit"
	"github.com/grailbio/base/retry"
	"github.com/grailbio/testutil/s3test"
)

const bucket = "test"

type file struct {
	content, sha256 string
}

func getFile(content string) file {
	return file{content: content, sha256: "not_really_sha256" + content}
}

func checkScan(t *testing.T, w *S3Walker, want []file) {
	t.Helper()
	var got []file
	for w.Scan(context.Background()) {
		got = append(got, file{aws.StringValue(w.Object().Key), *w.Metadata()["Content-Sha256"]})
	}
	if err := w.Err(); err != nil {
		t.Error(err)
	}
	sort.Slice(got, func(i, j int) bool { return got[i].content < got[j].content })
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func setup(t *testing.T) (client *s3test.Client, want []file) {
	t.Helper()
	client = s3test.NewClient(t, bucket)
	want = []file{getFile("test/x"), getFile("test/y"), getFile("test/z/foobar")}
	keys := append([]file{getFile("unrelated")}, want...)
	for _, key := range keys {
		client.SetFile(key.content, []byte(key.content), key.sha256)
	}
	return
}

func TestS3Walker(t *testing.T) {
	client, want := setup(t)
	w := &S3Walker{S3: client, Bucket: bucket, Prefix: "test/"}
	checkScan(t, w, want)
}

func TestS3WalkerRetries(t *testing.T) {
	rp := retry.MaxRetries(retry.Backoff(100*time.Millisecond, time.Minute, 1.5), 1)
	client, want := setup(t)
	client.Err = func(api string, input interface{}) error {
		if api != "ListObjectsV2Request" {
			return nil
		}
		lo, ok := input.(*s3.ListObjectsV2Input)
		if !ok {
			return nil
		}
		if !strings.HasPrefix(*lo.Prefix, "error") {
			return nil
		}
		return errors.New("some error")
	}
	w := &S3Walker{S3: client, Bucket: bucket, Prefix: "error/", Retrier: rp}
	if w.Scan(context.Background()) {
		t.Fatal("scan must fail")
	}
	if err := w.Err(); err == nil {
		t.Fatal("scan must fail")
	}
	w = &S3Walker{S3: client, Bucket: bucket, Prefix: "test/", Retrier: rp}
	checkScan(t, w, want)
}

func TestS3WalkerWithPolicy(t *testing.T) {
	rp := retry.MaxRetries(retry.Backoff(100*time.Millisecond, time.Minute, 1.5), 1)
	policy := admit.ControllerWithRetry(10, 10, rp)
	client, want := setup(t)
	w := &S3Walker{S3: client, Bucket: bucket, Prefix: "test/", Policy: policy, Retrier: rp}
	if err := policy.Acquire(context.Background(), 10); err != nil {
		t.Errorf("acquire failed!")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	// all tokens in use, so must get false
	if want, got := false, w.Scan(ctx); got != want {
		t.Fatalf("got %v, want %v", got, want)
	}
	policy.Release(10, true)
	// Setup new S3Walker with same policy (previous will be in err state).
	w = &S3Walker{S3: client, Bucket: bucket, Prefix: "test/", Policy: policy, Retrier: rp}
	checkScan(t, w, want)
}

func TestS3WalkerFile(t *testing.T) {
	client := s3test.NewClient(t, bucket)
	const key1, key2 = "path/to/a/file", "path/to/another/file"
	client.SetFile(key1, []byte("contents"), "sha256")
	client.SetFile(key2, []byte("other contents"), "another_sha256")
	client.Err = func(api string, input interface{}) error {
		if api != "HeadObject" {
			return nil
		}
		if input, ok := input.(*s3.HeadObjectInput); ok {
			if *input.Key == key2 {
				return awserr.New(s3.ErrCodeNoSuchKey, "test", nil)
			}
		}
		return nil
	}
	ctx := context.Background()
	w := &S3Walker{S3: client, Bucket: bucket, Prefix: "path/to/"}
	var got []file
	for w.Scan(ctx) {
		f := file{content: aws.StringValue(w.Object().Key)}
		if len(w.Metadata()) > 0 {
			f.sha256 = *w.Metadata()["Content-Sha256"]
		}
		got = append(got, f)
	}
	if err := w.Err(); err != nil {
		t.Error(err)
	}
	sort.Slice(got, func(i, j int) bool { return got[i].content < got[j].content })
	if want := []file{{key1, "sha256"}, {key2, ""}}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
