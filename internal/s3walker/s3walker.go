// Copyright 2017 GRAIL, Inc. All rights reserved.
// Use of this source code is governed by the Apache 2.0
// license that can be found in the LICENSE file.

package s3walker

import (
	"context"
	"log"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3iface"
	"github.com/grailbio/base/admit"
)

// S3Walker traverses s3 keys through a prefix scan.
type S3Walker struct {
	// S3 is the S3 client to be used.
	S3 s3iface.S3API
	// Bucket and Prefix name the location of the scan.
	Bucket, Prefix string

	// Admission policy for S3 operations (can be nil)
	Policy admit.Policy

	object  *s3.Object
	objects []*s3.Object
	token   *string
	err     error
	done    bool
}

// Scan scans the next key; it returns false when no more keys can
// be scanned, or if there was an error.
func (w *S3Walker) Scan(ctx context.Context) bool {
	if w.err != nil {
		return false
	}
	w.err = ctx.Err()
	if w.err != nil {
		return false
	}
	if len(w.objects) > 0 {
		w.object, w.objects = w.objects[0], w.objects[1:]
		return true
	}
	if w.done {
		return false
	}
	var res *s3.ListObjectsV2Output
	listObj := func() error {
		var req *request.Request
		req, res = w.S3.ListObjectsV2Request(&s3.ListObjectsV2Input{
			Bucket:            aws.String(w.Bucket),
			ContinuationToken: w.token,
			Prefix:            aws.String(w.Prefix),
		})
		req.HTTPRequest = req.HTTPRequest.WithContext(ctx)
		err := req.Send()
		if request.IsErrorThrottle(err) {
			log.Printf("s3walker.Scan: %s/%s: %v (over capacity)", w.Bucket, w.Prefix, err)
			return admit.ErrOverCapacity
		}
		return err
	}
	w.err = admit.Do(ctx, w.Policy, 1, listObj)
	if w.err != nil {
		return false
	}
	w.token = res.NextContinuationToken
	w.objects = res.Contents
	w.done = len(w.objects) == 0 || !aws.BoolValue(res.IsTruncated)
	return w.Scan(ctx)
}

// Err returns an error, if any.
func (w *S3Walker) Err() error {
	return w.err
}

// Object returns the last object that was scanned.
func (w *S3Walker) Object() *s3.Object {
	return w.object
}
