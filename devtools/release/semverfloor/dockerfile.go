package main

import (
	"strings"
)

// dockerfilePath is the one Dockerfile publish.yml builds every image from.
const dockerfilePath = "Dockerfile"

// publishedStageBodies returns, for every stage a published image is built
// from, the stage's instructions as one string. A stage is built from when it
// is a published image, is copied out of (`COPY --from=`), or is the base a
// stage chains from (`FROM stage`).
//
// Blank lines and comments are already gone, so two revisions that differ only
// there compare equal.
func publishedStageBodies(stages map[string]*dockerStage, images []string) map[string]string {
	out := map[string]string{}
	var walk func(name string)
	walk = func(name string) {
		s, ok := stages[name]
		if !ok {
			return
		}
		if _, seen := out[name]; seen {
			return
		}
		out[name] = strings.Join(s.body, "\n")
		for _, from := range s.copyFrom {
			walk(from)
		}
		walk(s.base)
	}
	for _, img := range images {
		walk(img)
	}
	return out
}

// dockerfileChanged reports whether a commit changes any stage a published
// image is built from. This is what puts the Dockerfile on the released
// surface: it compiles no package, so the package-directory test never sees
// it, yet a new base image or a changed build step changes what ships.
//
// Every case it cannot read for certain counts as a change, as the narrowing
// does: an added or deleted Dockerfile, a failed git read, or text that does
// not parse. A stage that is not published (a test fixture image) leaves the
// answer false.
func dockerfileChanged(root, sha string, images []string) bool {
	before, ok := gitShow(root, sha+"^:"+dockerfilePath)
	if !ok {
		return true
	}
	after, ok := gitShow(root, sha+":"+dockerfilePath)
	if !ok {
		return true
	}
	bs, err := parseDockerfileSource(dockerfilePath, string(before))
	if err != nil {
		return true
	}
	as, err := parseDockerfileSource(dockerfilePath, string(after))
	if err != nil {
		return true
	}
	return !sameBodies(publishedStageBodies(bs, images), publishedStageBodies(as, images))
}

func sameBodies(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}
