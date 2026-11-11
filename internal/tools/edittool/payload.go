package edittool

import (
	"context"
	"fmt"
	"strings"

	"github.com/pulseaiclub/phi/internal/tools/edittool/sloppy"
	"github.com/pulseaiclub/phi/internal/tools/tooldef"
)

// applySections applies a parsed payload to the files read through read, keyed
// by the payload's path spelling. Sections apply atomically: one failure and
// no file is written.
func applySections(
	sections []sloppy.Section,
	read func(path string) (string, error),
) (map[string]Result, error) {
	standalone := len(sections) == 1 && len(sections[0].Ops) == 1
	out := make(map[string]Result, len(sections))
	for _, section := range sections {
		content, err := read(section.Path)
		if err != nil {
			return nil, sectionError(section.Path, err, len(sections))
		}
		result, err := Apply(content, section, standalone)
		if err != nil {
			return nil, sectionError(section.Path, err, len(sections))
		}
		out[section.Path] = result
	}
	return out, nil
}

// sectionError attributes a failure to its file and, for multi-file payloads,
// states that no sibling was written. Single-file failures surface unwrapped.
func sectionError(path string, err error, sections int) error {
	detail := strings.TrimPrefix(err.Error(), atomicityNotice+"\n")
	if sections > 1 {
		detail = fmt.Sprintf("[%s]: %s\nNo files were modified — sections apply atomically.", path, detail)
	}
	return applyError{atomicityNotice + "\n" + detail}
}

// normalizeSections folds sections addressing the same file through different
// spellings (a.ts, ./a.ts, dir/../a.ts, an absolute path) into one section per
// absolute path, keeping first-seen order. Merged ops share a single anchor
// pass against the original content — the same guarantee the parser gives
// same-spelling sections it fuses itself. The returned map names the
// model-facing display path for every absolute target.
func normalizeSections(
	ctx context.Context,
	sections []sloppy.Section,
) ([]sloppy.Section, map[string]string, error) {
	merged := make([]sloppy.Section, 0, len(sections))
	displays := make(map[string]string, len(sections))
	position := make(map[string]int, len(sections))
	for _, section := range sections {
		abs, err := tooldef.ResolveToCwd(ctx, section.Path)
		if err != nil {
			return nil, nil, err
		}
		if pos, ok := position[abs]; ok {
			merged[pos].Ops = append(merged[pos].Ops, section.Ops...)
			continue
		}
		position[abs] = len(merged)
		displays[abs] = tooldef.RelToCwd(ctx, abs)
		merged = append(merged, sloppy.Section{Path: abs, Ops: section.Ops})
	}
	return merged, displays, nil
}
