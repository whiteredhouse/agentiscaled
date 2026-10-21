// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package console

import (
	"embed"
	"io/fs"
)

//go:embed all:public
var embeddedAssets embed.FS

// EmbeddedAssets returns the compiled-in console frontend, rooted at the
// asset directory so it can be used directly as Config.StaticFS.
func EmbeddedAssets() fs.FS {
	sub, err := fs.Sub(embeddedAssets, "public")
	if err != nil {
		// The subtree name is a compile-time constant matching the embed
		// directive; failure here is unreachable.
		panic(err)
	}
	return sub
}
