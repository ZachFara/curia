package install

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// skillMD is the agent-facing skill that explains Curia's decision tickets.
//
//go:embed skill/SKILL.md
var skillMD []byte

// skillMarker is the line that marks a SKILL.md as Curia's own.
const skillMarker = "<!-- curia:managed -->"

// skillName is the skill's directory under the skills directory.
const skillName = "curia-decisions"

// ErrForeignSkill is WriteSkill's error when SKILL.md exists without the
// marker line, so it is not Curia's and is left alone.
var ErrForeignSkill = errors.New("SKILL.md exists and is not Curia's (no " + skillMarker + " line); left alone")

// SkillPath is where WriteSkill puts the skill under skillsDir.
func SkillPath(skillsDir string) string {
	return filepath.Join(skillsDir, skillName, "SKILL.md")
}

// WriteSkill writes the embedded skill to skillsDir/curia-decisions/SKILL.md,
// atomically, creating the directories if needed. A file already there is
// replaced only if it holds the marker line; otherwise it is left alone and
// the error wraps ErrForeignSkill.
func WriteSkill(skillsDir string) error {
	path := filepath.Clean(SkillPath(skillsDir))
	ours, err := skillIsOurs(path)
	if errors.Is(err, fs.ErrNotExist) {
		return writeAtomic(path, skillMD)
	}
	if err != nil {
		return err
	}
	if !ours {
		return fmt.Errorf("%s: %w", path, ErrForeignSkill)
	}
	return writeAtomic(path, skillMD)
}

// RemoveSkill removes skillsDir/curia-decisions/SKILL.md if it holds the
// marker line, then the directory if that left it empty. A missing file, or
// one without the marker, is left alone and is not an error. A SKILL.md that
// is a symlink loses the link, never the file it points to.
func RemoveSkill(skillsDir string) error {
	path := filepath.Clean(SkillPath(skillsDir))
	ours, err := skillIsOurs(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil || !ours {
		return err
	}
	if err = os.Remove(path); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) > 0 {
		return err
	}
	return os.Remove(dir)
}

// skillIsOurs reports whether the file at path has the marker line.
func skillIsOurs(path string) (bool, error) {
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return false, err
	}
	for _, line := range bytes.Split(b, []byte("\n")) {
		if string(bytes.TrimRight(line, "\r")) == skillMarker {
			return true, nil
		}
	}
	return false, nil
}
