/*
   Copyright The containerd Authors.

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
   Copyright 2019 The Go Authors. All rights reserved.
   Use of this source code is governed by a BSD-style
   license that can be found in the NOTICE.md file.
*/

package reader

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/containerd/stargz-snapshotter/cache"
	"github.com/containerd/stargz-snapshotter/estargz"
	"github.com/containerd/stargz-snapshotter/metadata"
	tutil "github.com/containerd/stargz-snapshotter/util/testutil"
	"github.com/klauspost/compress/zstd"
	digest "github.com/opencontainers/go-digest"
	"golang.org/x/sync/errgroup"
)

type region struct{ b, e int64 }

const (
	sampleChunkSize    = 3
	sampleMiddleOffset = sampleChunkSize / 2
	sampleData1        = "0123456789"
	lastChunkOffset1   = sampleChunkSize * (int64(len(sampleData1)) / sampleChunkSize)
)

var srcCompressions = map[string]tutil.CompressionFactory{
	"zstd-fastest":               tutil.ZstdCompressionWithLevel(zstd.SpeedFastest),
	"gzip-bestspeed":             tutil.GzipCompressionWithLevel(gzip.BestSpeed),
	"externaltoc-gzip-bestspeed": tutil.ExternalTOCGzipCompressionWithLevel(gzip.BestSpeed),
}

// MockReadAtOutput defines the default output size for mocked read operations
var (
	MockReadAtOutput = 4194304
)

// TestingT is the minimal set of testing.T required to run the
// tests defined in TestSuiteReader. This interface exists to prevent
// leaking the testing package from being exposed outside tests.
type TestingT interface {
	Cleanup(func())
	Errorf(format string, args ...any)
	Fatal(args ...any)
	Fatalf(format string, args ...any)
	Logf(format string, args ...any)
}

// Runner allows running subtests of TestingT. This exists instead of adding
// a Run method to TestingT interface because the Run implementation of
// testing.T would not satisfy the interface.
type Runner func(t TestingT, name string, fn func(t TestingT))

type TestRunner struct {
	TestingT
	Runner Runner
}

func (r *TestRunner) Run(name string, run func(*TestRunner)) {
	r.Runner(r.TestingT, name, func(t TestingT) {
		run(&TestRunner{TestingT: t, Runner: r.Runner})
	})
}

func TestSuiteReader(t *TestRunner, store metadata.Store) {
	testFileReadAt(t, store)
	testCacheVerify(t, store)
	testFailReader(t, store)
	testPreReader(t, store)
	testProcessBatchChunks(t)
}

func testFileReadAt(t *TestRunner, factory metadata.Store) {
	sizeCond := map[string]int64{
		"single_chunk": sampleChunkSize - sampleMiddleOffset,
		"multi_chunks": sampleChunkSize + sampleMiddleOffset,
	}
	innerOffsetCond := map[string]int64{
		"at_top":    0,
		"at_middle": sampleMiddleOffset,
	}
	baseOffsetCond := map[string]int64{
		"of_1st_chunk":  sampleChunkSize * 0,
		"of_2nd_chunk":  sampleChunkSize * 1,
		"of_last_chunk": lastChunkOffset1,
	}
	fileSizeCond := map[string]int64{
		"in_1_chunk_file":  sampleChunkSize * 1,
		"in_2_chunks_file": sampleChunkSize * 2,
		"in_max_size_file": int64(len(sampleData1)),
	}
	cacheCond := map[string][]region{
		"with_clean_cache": nil,
		"with_edge_filled_cache": {
			region{0, sampleChunkSize - 1},
			region{lastChunkOffset1, int64(len(sampleData1)) - 1},
		},
		"with_sparse_cache": {
			region{0, sampleChunkSize - 1},
			region{2 * sampleChunkSize, 3*sampleChunkSize - 1},
		},
	}
	for sn, size := range sizeCond {
		for in, innero := range innerOffsetCond {
			for bo, baseo := range baseOffsetCond {
				for fn, filesize := range fileSizeCond {
					for cc, cacheExcept := range cacheCond {
						for srcCompressionName, srcCompression := range srcCompressions {
							srcCompression := srcCompression()
							t.Run(fmt.Sprintf("reading_%s_%s_%s_%s_%s_%s", sn, in, bo, fn, cc, srcCompressionName), func(t *TestRunner) {
								if filesize > int64(len(sampleData1)) {
									t.Fatal("sample file size is larger than sample data")
								}

								wantN := size
								offset := baseo + innero
								if remain := filesize - offset; remain < wantN {
									if wantN = remain; wantN < 0 {
										wantN = 0
									}
								}

								// use constant string value as a data source.
								want := strings.NewReader(sampleData1)

								// data we want to get.
								wantData := make([]byte, wantN)
								_, err := want.ReadAt(wantData, offset)
								if err != nil && err != io.EOF {
									t.Fatalf("want.ReadAt (offset=%d,size=%d): %v", offset, wantN, err)
								}

								// data we get through a file.
								f, closeFn := makeFile(t, []byte(sampleData1)[:filesize], sampleChunkSize, factory, srcCompression)
								defer closeFn()
								f.fr = newExceptFile(t, f.fr, cacheExcept...)
								for _, reg := range cacheExcept {
									id := genID(f.id, reg.b, reg.e-reg.b+1)
									w, err := f.gr.cache.Add(id)
									if err != nil {
										w.Close()
										t.Fatalf("failed to add cache %v: %v", id, err)
									}
									if _, err := w.Write([]byte(sampleData1[reg.b : reg.e+1])); err != nil {
										w.Close()
										t.Fatalf("failed to write cache %v: %v", id, err)
									}
									if err := w.Commit(); err != nil {
										w.Close()
										t.Fatalf("failed to commit cache %v: %v", id, err)
									}
									w.Close()
								}
								respData := make([]byte, size)
								n, err := f.ReadAt(respData, offset)
								if err != nil {
									t.Errorf("failed to read off=%d, size=%d, filesize=%d: %v", offset, size, filesize, err)
									return
								}
								respData = respData[:n]

								if !bytes.Equal(wantData, respData) {
									t.Errorf("off=%d, filesize=%d; read data{size=%d,data=%q}; want (size=%d,data=%q)",
										offset, filesize, len(respData), string(respData), wantN, string(wantData))
									return
								}

								// check cache has valid contents.
								cn := 0
								nr := 0
								for int64(nr) < wantN {
									chunkOffset, chunkSize, _, ok := f.fr.ChunkEntryForOffset(offset + int64(nr))
									if !ok {
										break
									}
									data := make([]byte, chunkSize)
									id := genID(f.id, chunkOffset, chunkSize)
									r, err := f.gr.cache.Get(id)
									if err != nil {
										t.Errorf("missed cache of offset=%d, size=%d: %v(got size=%d)", chunkOffset, chunkSize, err, n)
										return
									}
									defer r.Close()
									if n, err := r.ReadAt(data, 0); (err != nil && err != io.EOF) || n != int(chunkSize) {
										t.Errorf("failed to read cache of offset=%d, size=%d: %v(got size=%d)", chunkOffset, chunkSize, err, n)
										return
									}
									nr += n
									cn++
								}
							})
						}
					}
				}
			}
		}
	}
}

func newExceptFile(t TestingT, fr metadata.File, except ...region) metadata.File {
	er := exceptFile{fr: fr, t: t}
	er.except = map[region]bool{}
	for _, reg := range except {
		er.except[reg] = true
	}
	return &er
}

type exceptFile struct {
	fr     metadata.File
	except map[region]bool
	t      TestingT
}

func (er *exceptFile) ReadAt(p []byte, offset int64) (int, error) {
	if er.except[region{offset, offset + int64(len(p)) - 1}] {
		er.t.Fatalf("Requested prohibited region of chunk: (%d, %d)", offset, offset+int64(len(p))-1)
	}
	return er.fr.ReadAt(p, offset)
}

func (er *exceptFile) ChunkEntryForOffset(offset int64) (off int64, size int64, dgst string, ok bool) {
	return er.fr.ChunkEntryForOffset(offset)
}

func makeFile(t TestingT, contents []byte, chunkSize int, factory metadata.Store, comp tutil.Compression) (*file, func() error) {
	testName := "test"
	sr, dgst, err := tutil.BuildEStargz([]tutil.TarEntry{
		tutil.File(testName, string(contents)),
	}, tutil.WithEStargzOptions(estargz.WithChunkSize(chunkSize), estargz.WithCompression(comp)))
	if err != nil {
		t.Fatalf("failed to build sample estargz")
	}
	mr, err := factory(sr, metadata.WithDecompressors(comp))
	if err != nil {
		t.Fatalf("failed to create reader: %v", err)
	}
	vr, err := NewReader(mr, cache.NewMemoryCache(), digest.FromString(""))
	if err != nil {
		mr.Close()
		t.Fatalf("failed to make new reader: %v", err)
	}
	r, err := vr.VerifyTOC(dgst)
	if err != nil {
		vr.Close()
		t.Fatalf("failed to verify TOC: %v", err)
	}
	tid, _, err := r.Metadata().GetChild(r.Metadata().RootID(), testName)
	if err != nil {
		vr.Close()
		t.Fatalf("failed to get %q: %v", testName, err)
	}
	ra, err := r.OpenFile(tid)
	if err != nil {
		vr.Close()
		t.Fatalf("Failed to open testing file: %v", err)
	}
	f, ok := ra.(*file)
	if !ok {
		vr.Close()
		t.Fatalf("invalid type of file %q", tid)
	}
	return f, vr.Close
}

func testCacheVerify(t *TestRunner, factory metadata.Store) {
	for _, skipVerify := range [2]bool{true, false} {
		for _, invalidChunkBeforeVerify := range [2]bool{true, false} {
			for _, invalidChunkAfterVerify := range [2]bool{true, false} {
				for srcCompressionName, srcCompression := range srcCompressions {
					srcCompression := srcCompression()
					name := fmt.Sprintf("test_cache_verify_%v_%v_%v_%v",
						skipVerify, invalidChunkBeforeVerify, invalidChunkAfterVerify, srcCompressionName)
					t.Run(name, func(t *TestRunner) {
						sr, tocDgst, err := tutil.BuildEStargz([]tutil.TarEntry{
							tutil.File("a", sampleData1+"a"),
							tutil.File("b", sampleData1+"b"),
						}, tutil.WithEStargzOptions(estargz.WithChunkSize(sampleChunkSize), estargz.WithCompression(srcCompression)))
						if err != nil {
							t.Fatalf("failed to build sample estargz")
						}

						// Determine the expected behaviour
						var wantVerifyFail, wantCacheFail, wantCacheFail2 bool
						if skipVerify {
							// always no error if verification is disabled
							wantVerifyFail, wantCacheFail, wantCacheFail2 = false, false, false
						} else if invalidChunkBeforeVerify {
							// errors occurred before verifying TOC must be reported via VerifyTOC()
							wantVerifyFail = true
						} else if invalidChunkAfterVerify {
							// errors occurred after verifying TOC must be reported via Cache()
							wantVerifyFail, wantCacheFail, wantCacheFail2 = false, true, true
						} else {
							// otherwise no verification error
							wantVerifyFail, wantCacheFail, wantCacheFail2 = false, false, false
						}

						// Prepare reader
						verifier := &failIDVerifier{}
						mr, err := factory(sr, metadata.WithDecompressors(srcCompression))
						if err != nil {
							t.Fatalf("failed to prepare reader %v", err)
						}
						defer mr.Close()
						vr, err := NewReader(mr, cache.NewMemoryCache(), digest.FromString(""))
						if err != nil {
							t.Fatalf("failed to make new reader: %v", err)
						}
						vr.verifier = verifier.verifier
						vr.r.verifier = verifier.verifier

						off2id, id2path, err := prepareMap(vr.Metadata(), vr.Metadata().RootID(), "")
						if err != nil || off2id == nil || id2path == nil {
							t.Fatalf("failed to prepare offset map %v, off2id = %+v, id2path = %+v", err, off2id, id2path)
						}

						// Perform Cache() before verification
						// 1. Either of "a" or "b" is read and verified
						// 2. VerifyTOC/SkipVerify is called
						// 3. Another entry ("a" or "b") is called
						verifyDone := make(chan struct{})
						var firstEntryCalled bool
						var eg errgroup.Group
						var mu sync.Mutex
						eg.Go(func() error {
							return vr.Cache(WithFilter(func(off int64) bool {
								id, ok := off2id[off]
								if !ok {
									t.Fatalf("no ID is assigned to offset %d", off)
								}
								name, ok := id2path[id]
								if !ok {
									t.Fatalf("no name is assigned to id %d", id)
								}
								if name == "a" || name == "b" {
									mu.Lock()
									if !firstEntryCalled {
										firstEntryCalled = true
										if invalidChunkBeforeVerify {
											verifier.registerFails([]uint32{id})
										}
										mu.Unlock()
										return true
									}
									mu.Unlock()
									<-verifyDone
									if invalidChunkAfterVerify {
										verifier.registerFails([]uint32{id})
									}
									return true
								}
								return false
							}))
						})
						if invalidChunkBeforeVerify {
							// wait for encountering the error of the first chunk read
							start := time.Now()
							for {
								if err := vr.loadLastVerifyErr(); err != nil {
									break
								}
								if time.Since(start) > time.Second {
									t.Fatalf("timeout(1s): failed to wait for read error is registered")
								}
								time.Sleep(10 * time.Millisecond)
							}
						}

						// Perform verification
						if skipVerify {
							vr.SkipVerify()
						} else {
							_, err = vr.VerifyTOC(tocDgst)
						}
						if checkErr := checkError(wantVerifyFail, err); checkErr != nil {
							t.Errorf("verify: %v", checkErr)
							return
						}
						if err != nil {
							return
						}
						close(verifyDone)

						// Check the result of Cache()
						if checkErr := checkError(wantCacheFail, eg.Wait()); checkErr != nil {
							t.Errorf("cache: %v", checkErr)
							return
						}

						// Call Cache() again and check the result
						if checkErr := checkError(wantCacheFail2, vr.Cache()); checkErr != nil {
							t.Errorf("cache(2): %v", checkErr)
							return
						}
					})
				}
			}
		}
	}
}

type failIDVerifier struct {
	fails   []uint32
	failsMu sync.Mutex
}

func (f *failIDVerifier) registerFails(fails []uint32) {
	f.failsMu.Lock()
	defer f.failsMu.Unlock()
	f.fails = fails

}

func (f *failIDVerifier) verifier(id uint32, chunkDigest string) (digest.Verifier, error) {
	f.failsMu.Lock()
	defer f.failsMu.Unlock()
	success := true
	for _, n := range f.fails {
		if n == id {
			success = false
			break
		}
	}
	return &testVerifier{success}, nil
}

type testVerifier struct {
	success bool
}

func (bv *testVerifier) Write(p []byte) (n int, err error) {
	return len(p), nil
}

func (bv *testVerifier) Verified() bool {
	return bv.success
}

func checkError(wantFail bool, err error) error {
	if wantFail && err == nil {
		return fmt.Errorf("wanted to fail but succeeded")
	} else if !wantFail && err != nil {
		return fmt.Errorf("wanted to succeed verification but failed: %w", err)
	}
	return nil
}

func prepareMap(mr metadata.Reader, id uint32, p string) (off2id map[int64]uint32, id2path map[uint32]string, _ error) {
	attr, err := mr.GetAttr(id)
	if err != nil {
		return nil, nil, err
	}
	id2path = map[uint32]string{id: p}
	off2id = make(map[int64]uint32)
	if attr.Mode.IsRegular() {
		off, err := mr.GetOffset(id)
		if err != nil {
			return nil, nil, err
		}
		off2id[off] = id
	}
	var retErr error
	mr.ForeachChild(id, func(name string, id uint32, mode os.FileMode) bool {
		o2i, i2p, err := prepareMap(mr, id, path.Join(p, name))
		if err != nil {
			retErr = err
			return false
		}
		for k, v := range o2i {
			off2id[k] = v
		}
		for k, v := range i2p {
			id2path[k] = v
		}
		return true
	})
	if retErr != nil {
		return nil, nil, retErr
	}
	return off2id, id2path, nil
}

func testFailReader(t *TestRunner, factory metadata.Store) {
	testFileName := "test"
	for srcCompressionName, srcCompression := range srcCompressions {
		srcCompression := srcCompression()
		t.Run(fmt.Sprintf("%v", srcCompressionName), func(t *TestRunner) {
			for _, rs := range []bool{true, false} {
				for _, vs := range []bool{true, false} {
					stargzFile, tocDigest, err := tutil.BuildEStargz([]tutil.TarEntry{
						tutil.File(testFileName, sampleData1),
					}, tutil.WithEStargzOptions(estargz.WithChunkSize(sampleChunkSize), estargz.WithCompression(srcCompression)))
					if err != nil {
						t.Fatalf("failed to build sample estargz")
					}

					br := &breakReaderAt{
						ReaderAt: stargzFile,
						success:  true,
					}
					bev := &testChunkVerifier{true}
					mcache := cache.NewMemoryCache()
					mr, err := factory(io.NewSectionReader(br, 0, stargzFile.Size()), metadata.WithDecompressors(srcCompression))
					if err != nil {
						t.Fatalf("failed to prepare metadata reader")
					}
					defer mr.Close()
					vr, err := NewReader(mr, mcache, digest.FromString(""))
					if err != nil {
						t.Fatalf("failed to make new reader: %v", err)
					}
					defer vr.Close()
					vr.verifier = bev.verifier
					vr.r.verifier = bev.verifier
					gr, err := vr.VerifyTOC(tocDigest)
					if err != nil {
						t.Fatalf("failed to verify TOC: %v", err)
					}

					notexist := uint32(0)
					found := false
					for i := uint32(0); i < 1000000; i++ {
						if _, err := gr.Metadata().GetAttr(i); err != nil {
							notexist, found = i, true
							break
						}
					}
					if !found {
						t.Fatalf("free ID not found")
					}

					// tests for opening non-existing file
					_, err = gr.OpenFile(notexist)
					if err == nil {
						t.Errorf("succeeded to open file but wanted to fail")
						return
					}

					// tests failure behaviour of a file read
					tid, _, err := gr.Metadata().GetChild(gr.Metadata().RootID(), testFileName)
					if err != nil {
						t.Errorf("failed to get %q: %v", testFileName, err)
						return
					}
					fr, err := gr.OpenFile(tid)
					if err != nil {
						t.Errorf("failed to open file but wanted to succeed: %v", err)
						return
					}

					mcache.(*cache.MemoryCache).Membuf = map[string]*bytes.Buffer{}
					br.success = rs
					bev.success = vs

					// tests for reading file
					p := make([]byte, len(sampleData1))
					n, err := fr.ReadAt(p, 0)
					if rs && vs {
						if err != nil || n != len(sampleData1) || !bytes.Equal([]byte(sampleData1), p) {
							t.Errorf("failed to read data but wanted to succeed: %v", err)
							return
						}
					} else {
						if err == nil {
							t.Errorf("succeeded to read data but wanted to fail (reader:%v,verify:%v)", rs, vs)
							return
						}
					}
				}
			}
		})
	}
}

type breakReaderAt struct {
	io.ReaderAt
	success bool
}

func (br *breakReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if br.success {
		return br.ReaderAt.ReadAt(p, off)
	}
	return 0, fmt.Errorf("failed")
}

type testChunkVerifier struct {
	success bool
}

func (bev *testChunkVerifier) verifier(id uint32, chunkDigest string) (digest.Verifier, error) {
	return &testVerifier{bev.success}, nil
}

func testPreReader(t *TestRunner, factory metadata.Store) {
	randomData, err := tutil.RandomBytes(64000)
	if err != nil {
		t.Fatalf("failed rand.Read: %v", err)
	}
	data64KB := string(randomData)
	tests := []struct {
		name         string
		chunkSize    int
		minChunkSize int
		in           []tutil.TarEntry
		want         []check
	}{
		{
			name:         "several_files_in_chunk",
			minChunkSize: 8000,
			in: []tutil.TarEntry{
				tutil.Dir("foo/"),
				tutil.File("foo/foo1", data64KB),
				tutil.File("foo2", "bb"),
				tutil.File("foo22", "ccc"),
				tutil.Dir("bar/"),
				tutil.File("bar/bar.txt", "aaa"),
				tutil.File("foo3", data64KB),
			},
			// NOTE: we assume that the compressed "data64KB" is still larger than 8KB
			// landmark+dir+foo1, foo2+foo22+dir+bar.txt+foo3, TOC, footer
			want: []check{
				hasFileContentsWithPreCached("foo22", 0, "ccc", chunkInfo{"foo2", "bb", 0, 2}, chunkInfo{"bar/bar.txt", "aaa", 0, 3}, chunkInfo{"foo3", data64KB, 0, 64000}),
				hasFileContentsOffset("foo2", 0, "bb", true),
				hasFileContentsOffset("bar/bar.txt", 0, "aaa", true),
				hasFileContentsOffset("bar/bar.txt", 1, "aa", true),
				hasFileContentsOffset("bar/bar.txt", 2, "a", true),
				hasFileContentsOffset("foo3", 0, data64KB, true),
				hasFileContentsOffset("foo22", 0, "ccc", true),
				hasFileContentsOffset("foo/foo1", 0, data64KB, false),
				hasFileContentsOffset("foo/foo1", 0, data64KB, true),
				hasFileContentsOffset("foo/foo1", 1, data64KB[1:], true),
				hasFileContentsOffset("foo/foo1", 2, data64KB[2:], true),
				hasFileContentsOffset("foo/foo1", 3, data64KB[3:], true),
			},
		},
		{
			name:         "several_files_in_chunk_chunked",
			minChunkSize: 8000,
			chunkSize:    32000,
			in: []tutil.TarEntry{
				tutil.Dir("foo/"),
				tutil.File("foo/foo1", data64KB),
				tutil.File("foo2", "bb"),
				tutil.Dir("bar/"),
				tutil.File("foo3", data64KB),
			},
			// NOTE: we assume that the compressed chunk of "data64KB" is still larger than 8KB
			// landmark+dir+foo1(1), foo1(2), foo2+dir+foo3(1), foo3(2), TOC, footer
			want: []check{
				hasFileContentsWithPreCached("foo2", 0, "bb", chunkInfo{"foo3", data64KB[:32000], 0, 32000}),
				hasFileContentsOffset("foo2", 0, "bb", true),
				hasFileContentsOffset("foo2", 1, "b", true),
				hasFileContentsOffset("foo3", 0, data64KB[:len(data64KB)/2], true),
				hasFileContentsOffset("foo3", 1, data64KB[1:len(data64KB)/2], true),
				hasFileContentsOffset("foo3", 2, data64KB[2:len(data64KB)/2], true),
				hasFileContentsOffset("foo3", int64(len(data64KB)/2), data64KB[len(data64KB)/2:], false),
				hasFileContentsOffset("foo3", int64(len(data64KB)-1), data64KB[len(data64KB)-1:], true),
				hasFileContentsOffset("foo/foo1", 0, data64KB, false),
				hasFileContentsOffset("foo/foo1", 1, data64KB[1:], true),
				hasFileContentsOffset("foo/foo1", 2, data64KB[2:], true),
				hasFileContentsOffset("foo/foo1", int64(len(data64KB)/2), data64KB[len(data64KB)/2:], true),
				hasFileContentsOffset("foo/foo1", int64(len(data64KB)-1), data64KB[len(data64KB)-1:], true),
			},
		},
	}
	for _, tt := range tests {
		for srcCompresionName, srcCompression := range srcCompressions {
			srcCompression := srcCompression()
			t.Run(tt.name+"-"+srcCompresionName, func(t *TestRunner) {
				opts := []tutil.BuildEStargzOption{
					tutil.WithEStargzOptions(estargz.WithCompression(srcCompression)),
				}
				if tt.chunkSize > 0 {
					opts = append(opts, tutil.WithEStargzOptions(estargz.WithChunkSize(tt.chunkSize)))
				}
				if tt.minChunkSize > 0 {
					t.Logf("minChunkSize = %d", tt.minChunkSize)
					opts = append(opts, tutil.WithEStargzOptions(estargz.WithMinChunkSize(tt.minChunkSize)))
				}
				esgz, tocDgst, err := tutil.BuildEStargz(tt.in, opts...)
				if err != nil {
					t.Fatalf("failed to build sample eStargz: %v", err)
				}
				testR := &calledReaderAt{esgz, nil}
				mr, err := factory(io.NewSectionReader(testR, 0, esgz.Size()), metadata.WithDecompressors(srcCompression))
				if err != nil {
					t.Fatalf("failed to create new reader: %v", err)
				}
				defer mr.Close()
				memcache := cache.NewMemoryCache()
				vr, err := NewReader(mr, memcache, digest.FromString(""))
				if err != nil {
					t.Fatalf("failed to make new reader: %v", err)
				}
				rr, err := vr.VerifyTOC(tocDgst)
				if err != nil {
					t.Fatalf("failed to verify TOC: %v", err)
				}
				r := rr.(*reader)
				for _, want := range tt.want {
					want(t, r, testR)
				}
			})
		}
	}
}

type check func(TestingT, *reader, *calledReaderAt)

type chunkInfo struct {
	name        string
	data        string
	chunkOffset int64
	chunkSize   int64
}

func hasFileContentsOffset(name string, off int64, contents string, fromCache bool) check {
	return func(t TestingT, r *reader, cr *calledReaderAt) {
		tid, err := lookup(r, name)
		if err != nil {
			t.Fatalf("failed to lookup %q", name)
		}
		ra, err := r.OpenFile(tid)
		if err != nil {
			t.Fatalf("Failed to open testing file: %v", err)
		}
		cr.called = nil // reset test
		buf := make([]byte, len(contents))
		n, err := ra.ReadAt(buf, off)
		if err != nil {
			t.Fatalf("failed to readat %q: %v", name, err)
		}
		if n != len(contents) {
			t.Fatalf("failed to read contents %q (off:%d, want:%q) got %q", name, off, longBytesView([]byte(contents)), longBytesView(buf))
		}
		if string(buf) != contents {
			t.Fatalf("unexpected content of %q: %q want %q", name, longBytesView(buf), longBytesView([]byte(contents)))
		}
		t.Logf("reader calls for %q: offsets: %+v", name, cr.called)
		if fromCache {
			if len(cr.called) != 0 {
				t.Fatalf("unexpected read on %q: offsets: %v", name, cr.called)
			}
		} else {
			if len(cr.called) == 0 {
				t.Fatalf("no call happened to reader for %q", name)
			}
		}
	}
}

func hasFileContentsWithPreCached(name string, off int64, contents string, extra ...chunkInfo) check {
	return func(t TestingT, r *reader, cr *calledReaderAt) {
		tid, err := lookup(r, name)
		if err != nil {
			t.Fatalf("failed to lookup %q", name)
		}
		ra, err := r.OpenFile(tid)
		if err != nil {
			t.Fatalf("Failed to open testing file: %v", err)
		}
		buf := make([]byte, len(contents))
		n, err := ra.ReadAt(buf, off)
		if err != nil {
			t.Fatalf("failed to readat %q: %v", name, err)
		}
		if n != len(contents) {
			t.Fatalf("failed to read contents %q (off:%d, want:%q) got %q", name, off, longBytesView([]byte(contents)), longBytesView(buf))
		}
		if string(buf) != contents {
			t.Fatalf("unexpected content of %q: %q want %q", name, longBytesView(buf), longBytesView([]byte(contents)))
		}
		for _, e := range extra {
			eid, err := lookup(r, e.name)
			if err != nil {
				t.Fatalf("failed to lookup %q", e.name)
			}
			cacheID := genID(eid, e.chunkOffset, e.chunkSize)
			er, err := r.cache.Get(cacheID)
			if err != nil {
				t.Fatalf("failed to get cache %q: %+v", cacheID, e)
			}
			data, err := io.ReadAll(io.NewSectionReader(er, 0, e.chunkSize))
			er.Close()
			if err != nil {
				t.Fatalf("failed to read cache %q: %+v", cacheID, e)
			}
			if string(data) != e.data {
				t.Fatalf("unexpected contents of cache %q (%+v): %q; wanted %q", cacheID, e, longBytesView(data), longBytesView([]byte(e.data)))
			}
		}
	}
}

func lookup(r *reader, name string) (uint32, error) {
	name = strings.TrimPrefix(path.Clean("/"+name), "/")
	if name == "" {
		return r.Metadata().RootID(), nil
	}
	dir, base := filepath.Split(name)
	pid, err := lookup(r, dir)
	if err != nil {
		return 0, err
	}
	id, _, err := r.Metadata().GetChild(pid, base)
	return id, err
}

type calledReaderAt struct {
	io.ReaderAt
	called []int64
}

func (r *calledReaderAt) ReadAt(p []byte, off int64) (int, error) {
	r.called = append(r.called, off)
	return r.ReaderAt.ReadAt(p, off)
}

// longBytesView is an alias of []byte suitable for printing a long data as an omitted string to avoid long data being printed.
type longBytesView []byte

func (b longBytesView) String() string {
	if len(b) < 100 {
		return string(b)
	}
	return string(b[:50]) + "...(omit)..." + string(b[len(b)-50:])
}

func makeMockFile(id uint32) *file {
	mockCache := &mockCache{
		getError: fmt.Errorf("mock cache get error"),
	}

	mockFile := &mockFile{}

	gr := &reader{
		cache: mockCache,
	}

	return &file{
		id: id,
		fr: mockFile,
		gr: gr,
	}
}

type mockCache struct {
	getError error
}

func (c *mockCache) Add(key string, opts ...cache.Option) (cache.Writer, error) {
	return nil, fmt.Errorf("not implemented")
}

func (c *mockCache) Get(key string, opts ...cache.Option) (cache.Reader, error) {
	return nil, c.getError
}

func (c *mockCache) Close() error {
	return nil
}

type mockFile struct{}

func (f *mockFile) ChunkEntryForOffset(offset int64) (off int64, size int64, dgst string, ok bool) {
	return 0, 0, "", true
}

func (f *mockFile) ReadAt(p []byte, offset int64) (int, error) {
	return MockReadAtOutput, nil
}

func testProcessBatchChunks(t *TestRunner) {
	type testCase struct {
		name               string
		setupMock          func()
		createChunks       func(chunkSize int64, totalChunks int) []chunkData
		expectErrorInHoles bool
	}

	runTest := func(t TestingT, tc testCase) {
		if tc.setupMock != nil {
			tc.setupMock()
		}

		sf := makeMockFile(1)

		const (
			bufferSize  int64 = 400 * 1024 * 1024
			chunkSize   int64 = 4 * 1024 * 1024
			workerCount int   = 10
			totalChunks int   = 100
		)

		chunks := tc.createChunks(chunkSize, totalChunks)
		buffer := make([]byte, bufferSize)
		allReadInfos := make([][]chunkReadInfo, workerCount)
		eg := errgroup.Group{}

		for i := 0; i < workerCount && i < len(chunks); i++ {
			workerID := i
			args := &batchWorkerArgs{
				workerID:    workerID,
				chunks:      chunks,
				buffer:      buffer,
				workerCount: workerCount,
			}
			eg.Go(func() error {
				err := sf.processBatchChunks(args)
				if err == nil && len(args.readInfos) > 0 {
					allReadInfos[args.workerID] = args.readInfos
				}
				return err
			})
		}

		if err := eg.Wait(); err != nil {
			t.Fatalf("processBatchChunks failed: %v", err)
		}

		var mergedReadInfos []chunkReadInfo
		for _, infos := range allReadInfos {
			mergedReadInfos = append(mergedReadInfos, infos...)
		}

		err := sf.checkHoles(mergedReadInfos, bufferSize)
		if tc.expectErrorInHoles {
			if err == nil {
				t.Fatalf("checkHoles should have detected issues but didn't")
			}
			t.Logf("Expected error detected: %v", err)
		} else {
			if err != nil {
				t.Fatalf("checkHoles failed: %v", err)
			}
		}
	}

	createNormalChunks := func(chunkSize int64, totalChunks int) []chunkData {
		var chunks []chunkData
		for i := 0; i < totalChunks; i++ {
			chunks = append(chunks, chunkData{
				offset:    int64(i) * chunkSize,
				size:      chunkSize,
				digestStr: fmt.Sprintf("sha256:%d", i),
				bufferPos: int64(i) * chunkSize,
			})
		}
		return chunks
	}

	createOverlappingChunks := func(chunkSize int64, totalChunks int) []chunkData {
		chunks := createNormalChunks(chunkSize, totalChunks)

		for i := 0; i < totalChunks; i++ {
			if i > 0 && i%10 == 0 {
				chunks = append(chunks, chunkData{
					offset:    int64(i)*chunkSize - chunkSize/2,
					size:      chunkSize,
					digestStr: fmt.Sprintf("sha256:overlap-%d", i),
					bufferPos: int64(i) * chunkSize,
				})

				if i < totalChunks-1 {
					chunks = append(chunks, chunkData{
						offset:    int64(i+1)*chunkSize + chunkSize/2,
						size:      chunkSize,
						digestStr: fmt.Sprintf("sha256:gap-%d", i),
						bufferPos: int64(i+2) * chunkSize,
					})
				}
			}
		}
		return chunks
	}

	tests := []testCase{
		{
			name:               "test_process_batch_chunks_and_check_holes",
			createChunks:       createNormalChunks,
			expectErrorInHoles: false,
		},
		{
			name: "test_process_batch_chunks_with_holes",
			setupMock: func() {
				originalMockReadAtOutput := MockReadAtOutput
				MockReadAtOutput = MockReadAtOutput / 2
				t.Cleanup(func() {
					MockReadAtOutput = originalMockReadAtOutput
				})
			},
			createChunks:       createNormalChunks,
			expectErrorInHoles: true,
		},
		{
			name:               "test_process_batch_chunks_with_overlapping",
			createChunks:       createOverlappingChunks,
			expectErrorInHoles: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *TestRunner) {
			runTest(t, tc)
		})
	}
}
