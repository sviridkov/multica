package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
)

// Whole-root GC has no proof that a checkout, transcript or result can be
// reconstructed. Only empty scaffolding and understood identity metadata may
// be removed. In particular, clean/pushed Git and an old .task_lock are retained.
type emptyTaskRoot struct {
	base, parent, root   *os.Root
	basePath, workspace  string
	baseInfo, parentInfo os.FileInfo
	name                 string
	info                 os.FileInfo
	owner                execenv.EnvRootOwner
	files                map[string]gcControlFile
	dirs                 map[string]os.FileInfo
}

type gcControlFile struct {
	info os.FileInfo
	data []byte
}

func (p *emptyTaskRoot) close() {
	p.root.Close()
	p.parent.Close()
	p.base.Close()
}

// openGCSubdir rejects links (including Windows junctions) and pins the
// directory whose identity was checked, rather than following its name later.
func openGCSubdir(parent *os.Root, name string) (*os.Root, os.FileInfo, error) {
	info, err := parent.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if !info.IsDir() || info.Mode()&linkedDirModes != 0 {
		return nil, nil, fmt.Errorf("%s is not a plain directory", name)
	}
	root, err := parent.OpenRoot(name)
	if err != nil {
		return nil, nil, err
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		root.Close()
		return nil, nil, fmt.Errorf("%s changed while opening", name)
	}
	return root, info, nil
}

func readGCControl(root *os.Root, name string) (gcControlFile, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return gcControlFile{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > 64*1024 {
		return gcControlFile{}, fmt.Errorf("%s is not bounded regular metadata", name)
	}
	f, err := root.Open(name)
	if err != nil {
		return gcControlFile{}, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return gcControlFile{}, fmt.Errorf("%s changed while opening", name)
	}
	data, err := io.ReadAll(io.LimitReader(f, 64*1024+1))
	if err != nil {
		return gcControlFile{}, err
	}
	if len(data) > 64*1024 {
		return gcControlFile{}, fmt.Errorf("%s exceeds metadata limit", name)
	}
	return gcControlFile{info: info, data: data}, nil
}

func decodeGCControl(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("trailing metadata content")
	}
	return nil
}

func inspectEmptyTaskRoot(workspacesRoot, taskDir string) (_ *emptyTaskRoot, err error) {
	rel, err := filepath.Rel(workspacesRoot, taskDir)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if !filepath.IsLocal(rel) || len(parts) != 2 {
		return nil, errors.New("task path is not two levels inside workspaces root")
	}
	base, err := os.OpenRoot(workspacesRoot)
	if err != nil {
		return nil, err
	}
	baseInfo, err := base.Stat(".")
	if err != nil {
		base.Close()
		return nil, err
	}
	parent, parentInfo, err := openGCSubdir(base, parts[0])
	if err != nil {
		base.Close()
		return nil, err
	}
	root, info, err := openGCSubdir(parent, parts[1])
	if err != nil {
		parent.Close()
		base.Close()
		return nil, err
	}
	p := &emptyTaskRoot{
		base: base, parent: parent, root: root,
		basePath: workspacesRoot, workspace: parts[0], name: parts[1],
		baseInfo: baseInfo, parentInfo: parentInfo, info: info,
		files: map[string]gcControlFile{}, dirs: map[string]os.FileInfo{},
	}
	defer func() {
		if err != nil {
			p.close()
		}
	}()
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		name := entry.Name()
		switch name {
		case ".task_owner", ".gc_meta.json":
			file, readErr := readGCControl(root, name)
			if readErr != nil {
				return nil, readErr
			}
			p.files[name] = file
		case "workdir", "logs", "output":
			sub, subInfo, openErr := openGCSubdir(root, name)
			if openErr != nil {
				return nil, openErr
			}
			children, readErr := fs.ReadDir(sub.FS(), ".")
			sub.Close()
			if readErr != nil {
				return nil, readErr
			}
			if len(children) != 0 {
				return nil, fmt.Errorf("%s contains retained work or artifacts", name)
			}
			p.dirs[name] = subInfo
		default:
			return nil, fmt.Errorf("retained entry %s has no whole-root reclamation contract", name)
		}
	}
	ownerFile, ok := p.files[".task_owner"]
	if !ok {
		return nil, errors.New("task owner is missing")
	}
	ownerText := strings.TrimSpace(string(ownerFile.data))
	if strings.HasPrefix(ownerText, "{") {
		if err := decodeGCControl(ownerFile.data, &p.owner); err != nil {
			return nil, fmt.Errorf("invalid owner: %w", err)
		}
	} else {
		p.owner.TaskID = ownerText
	}
	if file, ok := p.files[".gc_meta.json"]; ok {
		var meta execenv.GCMeta
		if err := decodeGCControl(file.data, &meta); err != nil {
			return nil, fmt.Errorf("invalid GC metadata: %w", err)
		}
		if p.owner.WorkspaceID == "" {
			p.owner.WorkspaceID = meta.WorkspaceID
		}
		if meta.WorkspaceID != p.owner.WorkspaceID || (meta.TaskID != "" && meta.TaskID != p.owner.TaskID) {
			return nil, errors.New("GC metadata does not match task owner")
		}
		if meta.LocalDirectory {
			return nil, errors.New("local-directory environment is retained")
		}
		switch meta.Kind {
		case "", execenv.GCKindIssue, execenv.GCKindChat, execenv.GCKindAutopilotRun, execenv.GCKindQuickCreate:
		default:
			return nil, errors.New("unknown GC metadata kind")
		}
	}
	if err := execenv.ValidateEnvRootOwnerPath(workspacesRoot, taskDir, p.owner); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *emptyTaskRoot) revalidate() error {
	baseInfo, err := os.Stat(p.basePath)
	if err != nil {
		return err
	}
	parentInfo, err := p.base.Lstat(p.workspace)
	if err != nil {
		return err
	}
	if !os.SameFile(p.baseInfo, baseInfo) || parentInfo.Mode()&linkedDirModes != 0 || !os.SameFile(p.parentInfo, parentInfo) {
		return errors.New("workspace root identity changed")
	}
	current, err := p.parent.Lstat(p.name)
	if err != nil {
		return err
	}
	if current.Mode()&linkedDirModes != 0 || !os.SameFile(p.info, current) {
		return errors.New("task root identity changed")
	}
	entries, err := fs.ReadDir(p.root.FS(), ".")
	if err != nil {
		return err
	}
	if len(entries) != len(p.files)+len(p.dirs) {
		return errors.New("task contents changed")
	}
	for _, entry := range entries {
		name := entry.Name()
		if file, ok := p.files[name]; ok {
			current, err := readGCControl(p.root, name)
			if err != nil {
				return err
			}
			if !os.SameFile(file.info, current.info) || !bytes.Equal(file.data, current.data) {
				return fmt.Errorf("%s changed", name)
			}
		} else if info, ok := p.dirs[name]; ok {
			sub, current, err := openGCSubdir(p.root, name)
			if err != nil {
				return err
			}
			entries, readErr := fs.ReadDir(sub.FS(), ".")
			sub.Close()
			if readErr != nil {
				return readErr
			}
			if !os.SameFile(info, current) || len(entries) != 0 {
				return fmt.Errorf("%s changed or contains work", name)
			}
		} else {
			return fmt.Errorf("new retained entry %s", name)
		}
	}
	return nil
}

// remove is called under the daemon's env-root reservation. Individual rmdir
// operations cannot recursively consume files created after inspection.
func (p *emptyTaskRoot) remove() (reclaimed int64, err error) {
	removedFiles := map[string]gcControlFile{}
	defer func() {
		if err == nil {
			return
		}
		// A late writer may keep the root nonempty. Restore any removed control
		// files on the pinned root, without overwriting a concurrent replacement.
		for name, file := range removedFiles {
			f, restoreErr := p.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, file.info.Mode().Perm())
			if errors.Is(restoreErr, os.ErrExist) {
				continue
			}
			if restoreErr == nil {
				_, restoreErr = f.Write(file.data)
				restoreErr = errors.Join(restoreErr, f.Close())
			}
			if restoreErr != nil {
				err = errors.Join(err, fmt.Errorf("restore %s: %w", name, restoreErr))
			}
		}
	}()
	for name := range p.dirs {
		if err = p.revalidate(); err != nil {
			return 0, err
		}
		if err = removeGCEmptyDirectory(p.root, name); err != nil {
			return 0, err
		}
		delete(p.dirs, name)
	}
	for _, name := range []string{".gc_meta.json", ".task_owner"} {
		file, ok := p.files[name]
		if !ok {
			continue
		}
		if err = p.revalidate(); err != nil {
			return 0, err
		}
		if err = p.root.Remove(name); err != nil {
			return 0, err
		}
		removedFiles[name] = file
		delete(p.files, name)
		reclaimed += int64(len(file.data))
	}
	if err = p.revalidate(); err != nil {
		return 0, err
	}
	if err = removeGCEmptyDirectory(p.parent, p.name); err != nil {
		return 0, err
	}
	return reclaimed, nil
}
