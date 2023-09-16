package git

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/nicois/file"

	log "github.com/sirupsen/logrus"
)

type git struct {
	root            string
	defaultUpstream string
	treatAsTracked  []*regexp.Regexp
}

func (g *git) Run(args ...string) (string, error) {
	proc := exec.Command("git", args...)
	if b, err := proc.CombinedOutput(); err == nil {
		return string(b), nil
	} else {
		return "", err
	}
}

func (g *git) GetBranch() (string, error) {
	return g.Run("branch", "--show-current")
}

func (g *git) GetSha() (string, error) {
	/*
	   This does not check if the commit is dirty.
	*/
	return g.Run("rev-parse", "HEAD")
}

func (g *git) GetWorkingHash() (string, error) {
	/*
		A SHA based on both the git commit and any "dirty"
		changes made since that commit, whether staged or not.
	*/
	proc := exec.Command("git", "diff", "HEAD")
	out, err := proc.CombinedOutput()
	if err != nil {
		// probably not a git repo
		return "", err
	}
	hasher := sha256.New()
	hasher.Write(out)
	proc = exec.Command("git", "rev-parse", "HEAD")
	out, err = proc.CombinedOutput()
	if err != nil {
		return "", err
	}
	hasher.Write(out)
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func (g *git) GetChangedPaths(sinceRef string) file.Paths {
	// combine `git diff xxx...` and `git ls-files --modified`
	result := make(file.Paths)
	proc_diff := exec.Command("git", "diff", fmt.Sprintf("%v...", sinceRef), "--stat", "--name-only")
	proc_diff.Dir = g.root
	diff_output, err := proc_diff.CombinedOutput()
	if err != nil {
		log.Warn(err)
		return result
	}
	proc_ls := exec.Command("git", "ls-files", "--modified")
	proc_ls.Dir = g.root
	ls_output, err := proc_ls.CombinedOutput()
	if err != nil {
		log.Warn(err)
		return result
	}
	for _, path := range strings.Split(string(diff_output)+string(ls_output), "\n") {
		path = strings.TrimSpace(path)
		if len(path) > 0 {
			if path, err := filepath.Abs(filepath.Join(g.root, path)); err == nil {
				result.Add(path)
			}
		}
	}

	return result
}

func (g *git) IsTracked(path string) bool {
	relative_path, err := filepath.Rel(g.root, path)
	if err != nil {
		log.Warningf("%v is not inside %v", path, g.root)
	} else {
		for _, regex := range g.treatAsTracked {
			if regex.Match([]byte(relative_path)) {
				return true
			}
		}
	}
	proc := exec.Command("git", "ls-files", "--error-unmatch", relative_path)
	err = proc.Run()
	return err == nil
}

func (g *git) IsIgnored(path string) bool {
	proc := exec.Command("git", "check-ignore", path)
	err := proc.Run()
	return err == nil
}

func (g *git) GetRoot() string {
	return g.root
}

func Create(pathInRepo string) (*git, error) {
	path, err := filepath.EvalSymlinks(pathInRepo)
	if err != nil {
		return nil, err
	}
	path, err = filepath.Abs(pathInRepo)
	if err != nil {
		return nil, err
	}
	for {
		if path == "/" {
			return nil, fmt.Errorf("No git project root was found, starting at %v", pathInRepo)
		}
		if file.PathExists(filepath.Join(path, ".git")) {
			// os.Chdir(g.GetRoot())
			defaultUpstream := calculateDefaultUpstream(path)
			root, err := filepath.EvalSymlinks(path)
			if err != nil {
				return nil, err
			}
			treatAsTracked := getTreatAsTracked(root)
			return &git{root: root, defaultUpstream: defaultUpstream, treatAsTracked: treatAsTracked}, nil
		}
		path = filepath.Dir(path)
	}
}

func getTreatAsTracked(gitRoot string) []*regexp.Regexp {
	result := make([]*regexp.Regexp, 0)
	configFilename := filepath.Join(gitRoot, "._treat_as_tracked")
	content, err := file.ReadBytes(configFilename)
	if err == nil {
		for _, line := range strings.Split(string(content), "\n") {
			if line == "" {
				continue
			}
			log.Debugf("Treating files matching regex '%+v' as though they are tracked", line)
			if re, err := regexp.Compile(line); err == nil {
				result = append(result, re)
			} else {
				log.Warningf("Could not compile %v in %v: %+v", line, configFilename, err)
			}
		}
	}
	return result
}

func calculateDefaultUpstream(root string) string {
	candidates := []string{"origin/main", "origin/master"}
	if env := os.Getenv("GIT_DEFAULT_UPSTREAM"); len(env) > 0 {
		return strings.TrimSpace(env)
	}
	args := append([]string{"branch", "--list", "--remote"}, candidates...)
	proc := exec.Command("git", args...)
	proc.Dir = root
	output, err := proc.CombinedOutput()
	if err != nil {
		return ""
	}
	found := strings.Split(string(output), "\n")
	if len(found[0]) == 0 {
		log.Warningf("No upstream branches could be detected from %v, such as 'origin/main'. You will need to provide a valid branch name to use via GIT_DEFAULT_UPSTREAM", candidates)
		return ""
	}
	return strings.TrimSpace(found[0])
}

type Git interface {
	GetBranch() (string, error)
	GetWorkingHash() (string, error)
	GetChangedPaths(sinceRef string) file.Paths
	IsIgnored(path string) bool
	IsTracked(path string) bool
	GetRoot() (path string)
	DetectBranchChange(notify chan<- string)
	GetDefaultUpstream() string
}

func (g *git) GetDefaultUpstream() string {
	return g.defaultUpstream
}

func (g *git) DetectBranchChange(notify chan<- string) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}
	defer watcher.Close()
	branch, err := g.GetBranch()
	if err != nil {
		log.Fatal(err)
	}
	notify <- branch
	watcher.Add(filepath.Join(g.root, ".git"))
	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if event.Has(fsnotify.Write) {
				// flush any extra events which have accrued
			loop2:
				for {
					select {
					case <-watcher.Events:
					default:
						break loop2
					}
				}
				time.Sleep(time.Millisecond * 100)
				newBranch, err := g.GetBranch()
				if err != nil {
					log.Fatal(err)
				}
				if newBranch != branch {
					branch = newBranch
					notify <- branch
				}
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Error(err)
		}
	}
}
