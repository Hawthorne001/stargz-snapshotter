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

package memory

import (
	"io"
	"testing"

	"github.com/containerd/stargz-snapshotter/metadata"
	"github.com/containerd/stargz-snapshotter/metadata/testutil"
)

func TestReader(t *testing.T) {
	testRunner := &testutil.TestRunner{
		TestingT: t,
		Runner: func(testingT testutil.TestingT, name string, run func(t testutil.TestingT)) {
			tt, ok := testingT.(*testing.T)
			if !ok {
				testingT.Fatal("TestingT is not a *testing.T")
				return
			}

			tt.Run(name, func(t *testing.T) {
				run(t)
			})
		},
	}
	testutil.TestReader(testRunner, readerFactory)
}

func readerFactory(sr *io.SectionReader, opts ...metadata.Option) (testutil.TestableReader, error) {
	r, err := NewReader(sr, opts...)
	if err != nil {
		return nil, err
	}
	return r.(*reader), nil
}
