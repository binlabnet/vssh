package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ahmetb/go-linq"
	"github.com/mattn/go-shellwords"
	"github.com/mitchellh/go-homedir"
	"github.com/pkg/sftp"
	"github.com/scylladb/go-set/strset"
	"github.com/stephane-martin/vssh/lib"
	"golang.org/x/crypto/ssh/terminal"
)

type command func([]string) (string, error)

type cmpl func([]string) []string

type shellstate struct {
	LocalWD       string
	RemoteWD      string
	initRemoteWD  string
	client        *sftp.Client
	methods       map[string]command
	completes     map[string]cmpl
	externalPager bool
	info          func(string)
	err           func(string)
}

func newShellState(client *sftp.Client, externalPager bool, infoFunc func(string), errFunc func(string)) (*shellstate, error) {
	localwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	remotewd, err := client.Getwd()
	if err != nil {
		return nil, err
	}
	s := &shellstate{
		LocalWD:       localwd,
		RemoteWD:      remotewd,
		initRemoteWD:  remotewd,
		client:        client,
		externalPager: externalPager,
		info:          infoFunc,
		err:           errFunc,
	}
	s.methods = map[string]command{
		"less":      s.less,
		"lless":     s.lless,
		"ls":        s.ls,
		"lls":       s.lls,
		"ll":        s.ll,
		"lll":       s.lll,
		"cd":        s.cd,
		"lcd":       s.lcd,
		"exit":      s.exit,
		"logout":    s.exit,
		"pwd":       s.pwd,
		"lpwd":      s.lpwd,
		"get":       s.get,
		"put":       s.put,
		"mkdir":     s.mkdir,
		"mkdirall":  s.mkdirall,
		"lmkdir":    s.lmkdir,
		"lmkdirall": s.lmkdirall,
		"rm":        s.rm,
		"lrm":       s.lrm,
		"rmdir":     s.rmdir,
		"lrmdir":    s.lrmdir,
		"rename":    s.rename,
	}
	s.completes = map[string]cmpl{
		"cd":    s.completeCd,
		"lcd":   s.completeLcd,
		"less":  s.completeLess,
		"lless": s.completeLless,
	}
	return s, nil
}

func (s *shellstate) width() int {
	width, _, err := terminal.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return 80
	}
	return width
}

func (s *shellstate) exit(_ []string) (string, error) {
	return "", io.EOF
}

func (s *shellstate) complete(cmd string, args []string) []string {
	fun := s.completes[cmd]
	if fun == nil {
		return nil
	}
	return fun(args)
}

func (s *shellstate) dispatch(line string) (string, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", nil
	}

	p := shellwords.NewParser()
	args, err := p.Parse(line)
	if err != nil {
		return "", err
	}
	if p.Position != -1 {
		return "", errors.New("incomplete parsing error")
	}
	cmd := strings.ToLower(args[0])
	fun := s.methods[cmd]
	if fun == nil {
		return "", fmt.Errorf("unknown command: %s", cmd)
	}
	return fun(args[1:])
}

func join(dname, fname string) string {
	if strings.HasPrefix(fname, "/") {
		return fname
	}
	if strings.HasSuffix(fname, "/") {
		return filepath.Join(dname, fname) + "/"
	}
	return filepath.Join(dname, fname)
}

func (s *shellstate) rename(args []string) (string, error) {
	if len(args) != 2 {
		return "", errors.New("rename takes two arguments")
	}
	from := join(s.RemoteWD, args[0])
	to := join(s.RemoteWD, args[1])
	return "", s.client.Rename(from, to)
}

func (s *shellstate) mkdir(args []string) (string, error) {
	if len(args) == 0 {
		return "", errors.New("mkdir needs at least one argument")
	}
	for _, name := range args {
		path := join(s.RemoteWD, name)
		err := s.client.Mkdir(path)
		if err != nil {
			s.err(fmt.Sprintf("%s: %s", name, err))
		}
	}
	return "", nil
}

func (s *shellstate) rm(args []string) (string, error) {
	if len(args) == 0 {
		return "", errors.New("rm needs at least one argument")
	}
	for _, name := range args {
		path := join(s.RemoteWD, name)
		err := s.client.Remove(path)
		if err != nil {
			s.err(fmt.Sprintf("%s: %s", name, err))
		}
	}
	return "", nil
}

func (s *shellstate) rmdir(args []string) (string, error) {
	if len(args) == 0 {
		return "", errors.New("rmdir needs at least one argument")
	}
	var _rmdir func(string) error
	_rmdir = func(dirname string) (e error) {
		stats, err := s.client.Stat(dirname)
		if err != nil {
			return err
		}
		if !stats.IsDir() {
			return s.client.Remove(dirname)
		}
		files, err := s.client.ReadDir(dirname)
		if err != nil {
			return err
		}
		for _, file := range files {
			path := join(dirname, file.Name())
			if file.IsDir() {
				err := _rmdir(path)
				if err != nil {
					s.err(fmt.Sprintf("rmdir on %s: %s", path, err))
					if e == nil {
						e = err
					}
				}
			} else {
				err := s.client.Remove(path)
				if err != nil {
					s.err(fmt.Sprintf("rm on %s: %s", path, err))
					if e == nil {
						e = err
					}
				}
			}
		}
		if e != nil {
			return e
		}
		return s.client.Remove(dirname)
	}
	for _, name := range args {
		path := join(s.RemoteWD, name)
		err := _rmdir(path)
		if err != nil {
			s.err(fmt.Sprintf("%s: %s", name, err))
		}
	}
	return "", nil
}

func (s *shellstate) mkdirall(args []string) (string, error) {
	if len(args) == 0 {
		return "", errors.New("mkdirall needs at least one argument")
	}
	for _, name := range args {
		path := join(s.RemoteWD, name)
		err := s.client.MkdirAll(path)
		if err != nil {
			s.err(fmt.Sprintf("%s: %s", name, err))
		}
	}
	return "", nil
}

func (s *shellstate) lmkdir(args []string) (string, error) {
	if len(args) == 0 {
		return "", errors.New("lmkdir needs at least one argument")
	}
	for _, name := range args {
		path := join(s.LocalWD, name)
		err := os.Mkdir(path, 0755)
		if err != nil {
			s.err(fmt.Sprintf("%s: %s", name, err))
		}
	}
	return "", nil
}

func (s *shellstate) lrm(args []string) (string, error) {
	if len(args) == 0 {
		return "", errors.New("lrm needs at least one argument")
	}
	for _, name := range args {
		path := join(s.LocalWD, name)
		err := os.Remove(path)
		if err != nil {
			s.err(fmt.Sprintf("%s: %s", name, err))
		}
	}
	return "", nil
}

func (s *shellstate) lrmdir(args []string) (string, error) {
	if len(args) == 0 {
		return "", errors.New("lrmdir needs at least one argument")
	}
	for _, name := range args {
		path := join(s.LocalWD, name)
		err := os.RemoveAll(path)
		if err != nil {
			s.err(fmt.Sprintf("%s: %s", name, err))
		}
	}
	return "", nil
}

func (s *shellstate) lmkdirall(args []string) (string, error) {
	if len(args) == 0 {
		return "", errors.New("lmkdirall needs at least one argument")
	}
	for _, name := range args {
		path := join(s.LocalWD, name)
		err := os.MkdirAll(path, 0755)
		if err != nil {
			s.err(fmt.Sprintf("%s: %s", name, err))
		}
	}
	return "", nil
}

func (s *shellstate) less(args []string) (string, error) {
	if len(args) != 1 {
		return "", errors.New("less takes one argument")
	}
	fname := join(s.RemoteWD, args[0])
	f, err := s.client.Open(fname)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	return "", lib.ShowFile(fname, f, s.externalPager)
}

func (s *shellstate) lless(args []string) (string, error) {
	if len(args) != 1 {
		return "", errors.New("less takes one argument")
	}
	fname := join(s.LocalWD, args[0])
	f, err := os.Open(fname)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	return "", lib.ShowFile(fname, f, s.externalPager)
}

func (s *shellstate) get(args []string) (string, error) {
	remoteWD := s.RemoteWD
	if len(args) == 0 {
		names, err := lib.FuzzyRemote(s.client, remoteWD, nil)
		if err != nil {
			return "", err
		}
		if len(names) == 0 {
			return "", nil
		}
		args = names
	}
	var files, dirs []string
	for _, name := range args {
		path := join(remoteWD, name)
		stats, err := s.client.Stat(path)
		if err != nil {
			s.err(fmt.Sprintf("%s: %s", name, err))
			continue
		}
		if stats.IsDir() {
			dirs = append(dirs, path)
		} else if stats.Mode().IsRegular() {
			files = append(files, path)
		} else {
			s.err(fmt.Sprintf("not a regular file: %s", name))
		}
	}

	localWD := s.LocalWD
	for _, name := range dirs {
		err := s.getdir(localWD, name)
		if err != nil {
			s.err(fmt.Sprintf("download %s: %s", name, err))
		}
	}
	for _, name := range files {
		err := s.getfile(localWD, name)
		if err != nil {
			s.err(fmt.Sprintf("download %s: %s", name, err))
		}
	}
	return "", nil
}

func (s *shellstate) getfile(targetLocalDir, remoteFile string) error {
	source, err := s.client.Open(remoteFile)
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()

	localFilename := join(targetLocalDir, base(remoteFile))
	dest, err := os.Create(localFilename)
	if err != nil {
		return err
	}
	defer func() { _ = dest.Close() }()
	_, err = io.Copy(dest, source)
	if err != nil {
		return err
	}
	s.info(fmt.Sprintf("downloaded %s", remoteFile))
	return nil
}

func (s *shellstate) getdir(targetLocalDir, remoteDir string) error {
	files, err := s.client.ReadDir(remoteDir)
	if err != nil {
		return err
	}
	newDirname := join(targetLocalDir, base(remoteDir))
	err = os.Mkdir(newDirname, 0755)
	if err != nil && !os.IsExist(err) {
		return err
	}
	for _, f := range files {
		fname := join(remoteDir, f.Name())
		if f.IsDir() {
			err := s.getdir(newDirname, fname)
			if err != nil {
				s.err(fmt.Sprintf("download %s: %s", fname, err))
			}
		} else if f.Mode().IsRegular() {
			err := s.getfile(newDirname, fname)
			if err != nil {
				s.err(fmt.Sprintf("download %s: %s", fname, err))
			}
		}
	}
	s.info(fmt.Sprintf("downloaded %s", remoteDir))
	return nil
}

func (s *shellstate) put(args []string) (string, error) {
	localWD := s.LocalWD
	if len(args) == 0 {
		names, err := lib.FuzzyLocal(localWD, nil)
		if err != nil {
			return "", err
		}
		if len(names) == 0 {
			return "", nil
		}
		args = names
	}
	// check all files exist locally
	var files, dirs []string
	for _, name := range args {
		path := join(localWD, name)
		stats, err := os.Stat(path)
		if err != nil {
			s.err(fmt.Sprintf("%s: %s", name, err))
			continue
		}
		if stats.IsDir() {
			dirs = append(dirs, path)
		} else if stats.Mode().IsRegular() {
			files = append(files, path)
		} else {
			s.err(fmt.Sprintf("not a regular file: %s", name))
		}
	}
	remoteWD := s.RemoteWD
	for _, name := range dirs {
		err := s.putdir(remoteWD, name)
		if err != nil {
			s.err(fmt.Sprintf("upload %s: %s", name, err))
		}
	}
	for _, name := range files {
		err := s.putfile(remoteWD, name)
		if err != nil {
			s.err(fmt.Sprintf("upload %s: %s", name, err))
		}
	}
	return "", nil
}

func (s *shellstate) putfile(targetRemoteDir string, localFile string) error {
	remoteFilename := join(targetRemoteDir, base(localFile))
	source, err := os.Open(localFile)
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()
	dest, err := s.client.Create(remoteFilename)
	if err != nil {
		return err
	}
	defer func() { _ = dest.Close() }()
	_, err = io.Copy(dest, source)
	if err != nil {
		return err
	}
	s.info(fmt.Sprintf("uploaded %s", localFile))
	return nil
}

func (s *shellstate) putdir(targetRemoteDir, localDir string) error {
	files, err := ioutil.ReadDir(localDir)
	if err != nil {
		return err
	}
	newDirname := join(targetRemoteDir, base(localDir))
	err = s.client.Mkdir(newDirname)
	if err != nil && !os.IsExist(err) {
		return err
	}

	for _, f := range files {
		fname := join(localDir, f.Name())
		if f.IsDir() {
			err := s.putdir(newDirname, fname)
			if err != nil {
				s.err(fmt.Sprintf("upload %s: %s", fname, err))
			}
		} else if f.Mode().IsRegular() {
			err := s.putfile(newDirname, fname)
			if err != nil {
				s.err(fmt.Sprintf("upload %s: %s", fname, err))
			}
		}
	}
	s.info(fmt.Sprintf("uploaded %s", localDir))
	return nil
}

func (s *shellstate) pwd(args []string) (string, error) {
	if len(args) != 0 {
		return "", errors.New("pwd takes no argument")
	}
	return s.RemoteWD, nil
}

func (s *shellstate) lpwd(args []string) (string, error) {
	if len(args) != 0 {
		return "", errors.New("lpwd takes no argument")
	}
	return s.LocalWD, nil
}

func (s *shellstate) lcd(args []string) (string, error) {
	if len(args) > 1 {
		return "", errors.New("lcd takes only one argument")
	}
	if len(args) == 0 {
		name, err := homedir.Dir()
		if err != nil {
			return "", err
		}
		args = append(args, name)
	}
	d := join(s.LocalWD, args[0])
	stats, err := os.Stat(d)
	if err != nil {
		return "", err
	}
	if !stats.IsDir() {
		return "", errors.New("not a directory")
	}
	f, err := os.Open(d)
	_ = f.Close()
	if err != nil {
		return "", err
	}
	s.LocalWD = d
	return "", nil
}

func (s *shellstate) cd(args []string) (string, error) {
	if len(args) > 1 {
		return "", errors.New("cd takes only one argument")
	}
	if len(args) == 0 {
		args = append(args, s.initRemoteWD)
	}
	d := join(s.RemoteWD, args[0])
	stats, err := s.client.Stat(d)
	if err != nil {
		return "", err
	}
	if !stats.IsDir() {
		return "", errors.New("not a directory")
	}
	f, err := s.client.Open(d)
	if err != nil {
		return "", err
	}
	_ = f.Close()
	s.RemoteWD = d
	return "", nil
}

func findMatches(args []string, wd string, client *sftp.Client) (*strset.Set, error) {
	var glob func(string, string) ([]string, error)
	if client == nil {
		glob = lib.LocalGlob
	} else {
		glob = func(wd string, pattern string) ([]string, error) {
			return lib.SFTPGlob(wd, client, pattern)
		}
	}
	// no arg ==> list all files in current directory
	allmatches := strset.New()
	if len(args) == 0 {
		allmatches.Add(wd)
		return allmatches, nil
	}
	for _, pattern := range args {
		// list matching files
		matches, err := glob(wd, pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid pattern %s: %s", pattern, err)
		}
		for _, match := range matches {
			allmatches.Add(join(wd, match))
		}
	}
	return allmatches, nil
}

func _ls(wd string, width int, args []string, client *sftp.Client) (string, error) {
	var stat func(path string) (os.FileInfo, error)
	var readdir func(string) ([]os.FileInfo, error)
	if client == nil {
		stat = os.Stat
		readdir = ioutil.ReadDir
	} else {
		stat = client.Stat
		readdir = client.ReadDir
	}

	allmatches, err := findMatches(args, wd, client)
	if err != nil {
		return "", err
	}
	// map of directory ==> files
	files := make(map[string]*strset.Set)
	files["."] = strset.New()

	allmatches.Each(func(match string) bool {
		relMatch, err := filepath.Rel(wd, match)
		if err != nil {
			return true
		}

		stats, err := stat(match)
		if err != nil {
			return true
		}
		if stats.IsDir() {
			entries, err := readdir(match)
			if err != nil {
				return true
			}
			if _, ok := files[relMatch]; !ok {
				files[relMatch] = strset.New()
			}
			for _, entry := range entries {
				files[relMatch].Add(entry.Name())
			}
		} else {
			files["."].Add(relMatch)
		}
		return true
	})

	var buf strings.Builder
	printDirectory := func(d string, f *strset.Set) {
		if f.Size() == 0 {
			return
		}
		if d != "." {
			fmt.Fprintf(&buf, "%s:\n", d)
		}
		stats := make([]lib.Unixfile, 0, f.Size())
		names := f.List()
		sort.Strings(names)
		for _, fname := range names {
			s, err := stat(join(join(wd, d), fname))
			if err != nil {
				continue
			}
			stats = append(stats, lib.Unixfile{FileInfo: s, Path: fname})
		}
		lib.FormatListOfFiles(width, false, stats, &buf)
		fmt.Fprintln(&buf)
	}

	dirnames := make([]string, 0, len(files))
	for dirname := range files {
		dirnames = append(dirnames, dirname)
	}
	sort.Strings(dirnames)
	if f, ok := files["."]; ok {
		printDirectory(".", f)
	}
	for _, dirname := range dirnames {
		if dirname == "." {
			continue
		}
		printDirectory(dirname, files[dirname])
	}
	return buf.String(), nil
}

func (s *shellstate) lls(args []string) (string, error) {
	return _ls(s.LocalWD, s.width(), args, nil)
}

func (s *shellstate) ls(args []string) (string, error) {
	return _ls(s.RemoteWD, s.width(), args, s.client)
}

func (s *shellstate) lll(args []string) (string, error) {
	for {
		files, err := ioutil.ReadDir(s.LocalWD)
		if err != nil {
			return "", err
		}
		selected, err := lib.TableOfFiles(s.LocalWD, files, false)
		if err != nil {
			return "", err
		}
		if selected.Name == "" {
			return "", nil
		}
		if selected.Name == ".." {
			_, err := s.lcd([]string{".."})
			if err != nil {
				return "", err
			}
		} else if selected.Mode.IsDir() {
			_, err := s.lcd([]string{selected.Name})
			if err != nil {
				return "", err
			}
		} else {
			_, err := s.lless([]string{selected.Name})
			if err != nil {
				return "", err
			}
		}
	}
}

func (s *shellstate) ll(args []string) (string, error) {
	for {
		files, err := s.client.ReadDir(s.RemoteWD)
		if err != nil {
			return "", fmt.Errorf("error listing directory: %s", err)
		}
		selected, err := lib.TableOfFiles(s.RemoteWD, files, true)
		if err != nil {
			return "", err
		}
		if selected.Name == "" {
			return "", nil
		}
		if selected.Name == ".." {
			_, err := s.cd([]string{".."})
			if err != nil {
				return "", err
			}
		} else if selected.Mode.IsDir() {
			_, err := s.cd([]string{selected.Name})
			if err != nil {
				return "", err
			}
		} else {
			_, err := s.less([]string{selected.Name})
			if err != nil {
				return "", err
			}
		}
	}
}

func (s *shellstate) completeLess(args []string) []string {
	if len(args) > 1 {
		return nil
	}
	var input string
	if len(args) == 1 {
		input = args[0]
	}
	cand, dirname, relDirname := candidate(s.RemoteWD, input)
	files, err := s.client.ReadDir(dirname)
	if err != nil {
		return nil
	}

	props := completeFiles(cand, files, false, false)
	if len(props) == 0 {
		return nil
	}
	linq.From(props).SelectT(func(s string) string {
		return join(relDirname, s)
	}).ToSlice(&props)
	return props
}

func base(s string) string {
	s = filepath.Base(s)
	if s == "/" {
		return ""
	}
	return s
}

func candidate(wd, input string) (cand, dirname, relDirname string) {
	var err error
	if input == "" {
		return "", wd, ""
	}
	if strings.HasSuffix(input, "/") {
		cand = ""
		dirname = join(wd, input)
	} else {
		cand = base(input)
		dirname = filepath.Dir(join(wd, input))
	}
	relDirname = dirname
	if !strings.HasPrefix(input, "/") {
		relDirname, err = filepath.Rel(wd, dirname)
		if err != nil {
			relDirname = dirname
		}
	}
	return cand, dirname, relDirname
}

func (s *shellstate) completeLless(args []string) []string {
	if len(args) > 1 {
		return nil
	}
	var input string
	if len(args) == 1 {
		input = args[0]
	}
	cand, dirname, relDirname := candidate(s.LocalWD, input)
	files, err := ioutil.ReadDir(dirname)
	if err != nil {
		return nil
	}
	props := completeFiles(cand, files, false, false)
	if len(props) == 0 {
		return nil
	}
	linq.From(props).SelectT(func(s string) string {
		return join(relDirname, s)
	}).ToSlice(&props)
	return props
}

func (s *shellstate) completeLcd(args []string) []string {
	if len(args) > 1 {
		return nil
	}
	var input string
	if len(args) == 1 {
		input = args[0]
	}
	cand, dirname, relDirname := candidate(s.LocalWD, input)
	files, err := ioutil.ReadDir(dirname)
	if err != nil {
		return nil
	}
	props := completeFiles(cand, files, true, false)
	if len(props) == 0 {
		return nil
	}
	linq.From(props).SelectT(func(s string) string {
		return join(relDirname, s)
	}).ToSlice(&props)
	return props
}

func (s *shellstate) completeCd(args []string) []string {
	if len(args) > 1 {
		return nil
	}
	var input string
	if len(args) == 1 {
		input = args[0]
	}
	cand, dirname, relDirname := candidate(s.RemoteWD, input)
	files, err := s.client.ReadDir(dirname)
	if err != nil {
		return nil
	}
	props := completeFiles(cand, files, true, false)
	if len(props) == 0 {
		return nil
	}
	linq.From(props).SelectT(func(s string) string {
		return join(relDirname, s)
	}).ToSlice(&props)
	return props
}

func completeFiles(candidate string, files []os.FileInfo, onlyDirs, onlyFiles bool) []string {
	var props []string

	if onlyDirs {
		linq.From(files).
			WhereT(func(info os.FileInfo) bool {
				return info.IsDir()
			}).
			SelectT(func(info os.FileInfo) string { return info.Name() + "/" }).
			ToSlice(&props)
	} else if onlyFiles {
		linq.From(files).
			WhereT(func(info os.FileInfo) bool { return info.Mode().IsRegular() }).
			SelectT(func(info os.FileInfo) string { return info.Name() }).
			ToSlice(&props)
	} else {
		linq.From(files).
			SelectT(func(info os.FileInfo) string {
				if info.IsDir() {
					return info.Name() + "/"
				}
				return info.Name()
			}).
			ToSlice(&props)
	}
	if candidate != "" {
		linq.From(props).WhereT(func(s string) bool { return strings.HasPrefix(s, candidate) }).ToSlice(&props)
	}
	if len(props) == 0 {
		return nil
	}
	linq.From(props).SelectT(func(p string) string {
		var buf bytes.Buffer
		quote(p, &buf)
		return buf.String()
	}).ToSlice(&props)
	return props
}