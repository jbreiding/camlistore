/*
Copyright 2011 Google Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

/*
Package cond registers the "cond" conditional blobserver storage type
to select routing of get/put operations on blobs to other storage
targets as a function of their content.

Currently only the "isSchema" predicate is defined.

Example usage:

  "/bs-and-maybe-also-index/": {
	"handler": "storage-cond",
	"handlerArgs": {
		"write": {
			"if": "isSchema",
			"then": "/bs-and-index/",
			"else": "/bs/"
		},
		"read": "/bs/"
	}
  }
*/
package cond

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"camlistore.org/pkg/blob"
	"camlistore.org/pkg/blobserver"
	"camlistore.org/pkg/context"
	"camlistore.org/pkg/jsonconfig"
	"camlistore.org/pkg/schema"
)

const buffered = 8

type storageFunc func(src io.Reader) (dest blobserver.Storage, overRead []byte, err error)

type condStorage struct {
	storageForReceive storageFunc
	read              blobserver.Storage
	remove            blobserver.Storage

	ctx *http.Request // optional per-request context
}

func (sto *condStorage) StorageGeneration() (initTime time.Time, random string, err error) {
	if gener, ok := sto.read.(blobserver.Generationer); ok {
		return gener.StorageGeneration()
	}
	err = blobserver.GenerationNotSupportedError(fmt.Sprintf("blobserver.Generationer not implemented on %T", sto.read))
	return
}

func (sto *condStorage) ResetStorageGeneration() error {
	if gener, ok := sto.read.(blobserver.Generationer); ok {
		return gener.ResetStorageGeneration()
	}
	return blobserver.GenerationNotSupportedError(fmt.Sprintf("blobserver.Generationer not implemented on %T", sto.read))
}

func newFromConfig(ld blobserver.Loader, conf jsonconfig.Obj) (storage blobserver.Storage, err error) {
	sto := &condStorage{}

	receive := conf.OptionalStringOrObject("write")
	read := conf.RequiredString("read")
	remove := conf.OptionalString("remove", "")
	if err := conf.Validate(); err != nil {
		return nil, err
	}

	if receive != nil {
		sto.storageForReceive, err = buildStorageForReceive(ld, receive)
		if err != nil {
			return
		}
	}

	sto.read, err = ld.GetStorage(read)
	if err != nil {
		return
	}

	if remove != "" {
		sto.remove, err = ld.GetStorage(remove)
		if err != nil {
			return
		}
	}
	return sto, nil
}

func buildStorageForReceive(ld blobserver.Loader, confOrString interface{}) (storageFunc, error) {
	// Static configuration from a string
	if s, ok := confOrString.(string); ok {
		sto, err := ld.GetStorage(s)
		if err != nil {
			return nil, err
		}
		f := func(io.Reader) (blobserver.Storage, []byte, error) {
			return sto, nil, nil
		}
		return f, nil
	}

	conf := jsonconfig.Obj(confOrString.(map[string]interface{}))

	ifStr := conf.RequiredString("if")
	// TODO: let 'then' and 'else' point to not just strings but either
	// a string or a JSON object with another condition, and then
	// call buildStorageForReceive on it recursively
	thenTarget := conf.RequiredString("then")
	elseTarget := conf.RequiredString("else")
	if err := conf.Validate(); err != nil {
		return nil, err
	}
	thenSto, err := ld.GetStorage(thenTarget)
	if err != nil {
		return nil, err
	}
	elseSto, err := ld.GetStorage(elseTarget)
	if err != nil {
		return nil, err
	}

	switch ifStr {
	case "isSchema":
		return isSchemaPicker(thenSto, elseSto), nil
	}
	return nil, fmt.Errorf("cond: unsupported 'if' type of %q", ifStr)
}

// dummyRef is just a dummy reference to give to BlobFromReader.
var dummyRef = blob.MustParse("sha1-829c3804401b0727f70f73d4415e162400cbe57b")

func isSchemaPicker(thenSto, elseSto blobserver.Storage) storageFunc {
	return func(src io.Reader) (dest blobserver.Storage, overRead []byte, err error) {
		var buf bytes.Buffer
		tee := io.TeeReader(src, &buf)
		blob, err := schema.BlobFromReader(dummyRef, tee)
		if err != nil || blob.Type() == "" {
			return elseSto, buf.Bytes(), nil
		}
		return thenSto, buf.Bytes(), nil
	}
}

func (sto *condStorage) ReceiveBlob(br blob.Ref, source io.Reader) (sb blob.SizedRef, err error) {
	destSto, overRead, err := sto.storageForReceive(source)
	if err != nil {
		return
	}
	if len(overRead) > 0 {
		source = io.MultiReader(bytes.NewReader(overRead), source)
	}
	return blobserver.Receive(destSto, br, source)
}

func (sto *condStorage) RemoveBlobs(blobs []blob.Ref) error {
	if sto.remove != nil {
		return sto.remove.RemoveBlobs(blobs)
	}
	return errors.New("cond: Remove not configured")
}

func (sto *condStorage) IsFetcherASeeker() bool {
	_, ok := sto.read.(blob.SeekFetcher)
	return ok
}

func (sto *condStorage) FetchStreaming(b blob.Ref) (file io.ReadCloser, size int64, err error) {
	if sto.read != nil {
		return sto.read.FetchStreaming(b)
	}
	err = errors.New("cond: Read not configured")
	return
}

func (sto *condStorage) StatBlobs(dest chan<- blob.SizedRef, blobs []blob.Ref) error {
	if sto.read != nil {
		return sto.read.StatBlobs(dest, blobs)
	}
	return errors.New("cond: Read not configured")
}

func (sto *condStorage) EnumerateBlobs(ctx *context.Context, dest chan<- blob.SizedRef, after string, limit int) error {
	if sto.read != nil {
		return sto.read.EnumerateBlobs(ctx, dest, after, limit)
	}
	return errors.New("cond: Read not configured")
}

func init() {
	blobserver.RegisterStorageConstructor("cond", blobserver.StorageConstructor(newFromConfig))
}
