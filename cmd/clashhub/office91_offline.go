package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

var office91OfflineMu sync.Mutex

// No row order, mtime, or filename is evidence of account identity. Multiple
// local profiles require the caller to explicitly select an input file.
func discoverOffice91OfflineProfile(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", errors.New("offline-profile-unavailable")
	}
	var paths []string
	for _, e := range entries {
		if e.Type().IsRegular() && filepath.Ext(e.Name()) == ".yaml" {
			paths = append(paths, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		return "", errors.New("offline-profile-unavailable")
	}
	if len(paths) != 1 {
		return "", errors.New("offline-profile-identity-ambiguous")
	}
	return paths[0], nil
}

// No subscription fetch, configuration update or engine reload occurs here.
func importOffice91OfflineModule(ctx context.Context, profilePath, pythonPath, validatorPath, module, keyPath string) error {
	office91OfflineMu.Lock()
	defer office91OfflineMu.Unlock()
	if profilePath == "" {
		var err error
		profilePath, err = discoverOffice91OfflineProfile(filepath.Join(userHome(), "Library", "Application Support", "com.abysswalker.office91", "profiles"))
		if err != nil {
			return err
		}
	}
	if !filepath.IsAbs(profilePath) || !filepath.IsAbs(pythonPath) || !filepath.IsAbs(validatorPath) {
		return errors.New("offline-import-requires-absolute-paths")
	}
	target := filepath.Join(appSupportDir(), "sources", "office91.yaml")
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return errors.New("offline-import-randomness-unavailable")
	}
	temporary := ".office91-offline-" + hex.EncodeToString(nonce)
	// Parent cleanup also runs if cancellation kills Python before its finally.
	defer os.Remove(filepath.Join(filepath.Dir(target), temporary))
	cmd, err := office91PythonCommand(ctx, pythonPath, "-I", "-c", office91OfflineImporter, "embedded", profilePath, target, validatorPath, temporary)
	if err != nil {
		return err
	}
	cmd.Stdin = strings.NewReader(module)
	cmd.Env = append(os.Environ(), "CLASHHUB_OFFLINE_KEY_FILE="+keyPath)
	// Suppress all module/parser exceptions, URLs, nodes, keys and plaintext.
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("offline-profile-authentication-or-validation-failed")
	}
	return nil
}

const office91OfflineImporter = `
import base64, hashlib, os, pathlib, stat, sys, types
try:
    module=types.ModuleType("embedded_office91_crypto")
    exec(compile(sys.stdin.read(),"<embedded-office91-crypto>","exec"),module.__dict__)
    source=os.open(sys.argv[2],os.O_RDONLY|os.O_NOFOLLOW)
    with os.fdopen(source,"rb") as f:
        info=os.fstat(f.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_size>16*1024*1024: raise ValueError()
        encoded=f.read(16*1024*1024+1)
    raw=base64.b64decode(encoded.strip(),validate=True)
    if len(raw)<28: raise ValueError()
    plain=module.AESGCM(module.derive()).decrypt(raw[:12],raw[12:],None)
    doc=module.yaml.load(plain.decode("utf-8","strict"),Loader=module.UniqueLoader)
    if not isinstance(doc,dict): raise ValueError()
    nodes=doc.get("proxies")
    if not isinstance(nodes,list) or not nodes: raise ValueError()
    names=set()
    for node in nodes:
        if not isinstance(node,dict): raise ValueError()
        name=node.get("name");protocol=node.get("type");port=node.get("port")
        if not isinstance(name,str) or not name.strip() or name in names: raise ValueError()
        if not isinstance(protocol,str) or protocol.lower() not in module.KNOWN or protocol.lower()=="direct": raise ValueError()
        if not isinstance(node.get("server"),str) or not node["server"].strip(): raise ValueError()
        if type(port) is not int or not 1<=port<=65535: raise ValueError()
        names.add(name)
    # Only authenticated inline nodes enter the cache; no provider URLs, rules,
    # listeners, script, or controller settings from the input are imported.
    payload=module.yaml.safe_dump({"proxies":nodes},allow_unicode=True,sort_keys=False).encode()
    target=pathlib.Path(sys.argv[3]);parent=target.parent
    parent.mkdir(mode=0o700,parents=True,exist_ok=True)
    directory=os.open(parent,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW)
    tmp=None
    try:
        st=os.fstat(directory)
        if st.st_uid!=os.getuid() or stat.S_IMODE(st.st_mode)&0o077: raise ValueError()
        try:
            old=os.stat(target.name,dir_fd=directory,follow_symlinks=False)
            if not stat.S_ISREG(old.st_mode) or old.st_uid!=os.getuid(): raise ValueError()
        except FileNotFoundError: pass
        tmp=sys.argv[5]
        fd=os.open(tmp,os.O_RDWR|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600,dir_fd=directory)
        with os.fdopen(fd,"w+b") as f:
            f.write(payload);f.flush();os.fsync(f.fileno())
            identity=os.fstat(f.fileno())
            expected=hashlib.sha256(payload).digest()
            def verify_candidate():
                opened=os.fstat(f.fileno())
                named=os.stat(tmp,dir_fd=directory,follow_symlinks=False)
                parent_now=os.stat(parent,follow_symlinks=False)
                parent_fd=os.fstat(directory)
                if (parent_now.st_dev,parent_now.st_ino)!=(parent_fd.st_dev,parent_fd.st_ino): raise ValueError()
                if not stat.S_ISDIR(parent_now.st_mode) or parent_now.st_uid!=os.getuid() or stat.S_IMODE(parent_now.st_mode)&0o077: raise ValueError()
                for entry in (opened,named):
                    if (entry.st_dev,entry.st_ino)!=(identity.st_dev,identity.st_ino): raise ValueError()
                    if not stat.S_ISREG(entry.st_mode) or entry.st_uid!=os.getuid() or stat.S_IMODE(entry.st_mode)!=0o600 or entry.st_nlink!=1: raise ValueError()
                current=os.pread(f.fileno(),len(payload)+1,0)
                if len(current)!=len(payload) or hashlib.sha256(current).digest()!=expected: raise ValueError()
            verify_candidate()
            # Darwin has /dev/fd, not /proc/self/fd. Pass the held file itself
            # and bind child cwd by descriptor; no parent pathname is reopened
            # by Mihomo to select the candidate it actually validates.
            import subprocess
            f.seek(0)
            descriptor_path="/dev/fd/"+str(f.fileno())
            checked=subprocess.run([sys.argv[4],"-t","-d",".","-f",descriptor_path],preexec_fn=lambda:os.fchdir(directory),pass_fds=(directory,f.fileno()),stdin=subprocess.DEVNULL,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=20)
            if checked.returncode!=0: raise ValueError()
            verify_candidate()
            os.replace(tmp,target.name,src_dir_fd=directory,dst_dir_fd=directory);tmp=None
        os.fsync(directory)
    finally:
        if tmp is not None: os.unlink(tmp,dir_fd=directory)
        os.close(directory)
except BaseException:
    sys.exit(1)
`
