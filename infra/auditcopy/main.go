// Auditcopy keeps a copy of the gateway's audit log off the VM, on the host, and checks the VM's log
// against it. The hash chain in the VM shows that rows were not changed after the fact only if its
// later state is kept somewhere the database cannot reach (ADR 0015). Here every row is kept, so a row
// rewritten after it was copied, with the whole chain recomputed so that it still verifies in the VM,
// no longer matches the copy. It only reads the VM. See ADR 0024.
//
// Usage: auditcopy -root REPO -dir DIR
//
// It checks that every row already copied is unchanged in the VM, checks each new row's chain and
// hash, then writes the copy with the new rows. If a check fails, the copy is left as it was.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"airgapai/auditcopy/audit"
	"airgapai/gateway/approval"
)

func main() {
	root := flag.String("root", ".", "the repository")
	dir := flag.String("dir", "", "where the copies are kept, one file per database")
	flag.Parse()
	if *dir == "" || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: auditcopy -root REPO -dir DIR")
		os.Exit(2)
	}
	if err := run(*root, *dir); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL", err)
		os.Exit(1)
	}
}

func run(root, dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// One run at a time: two would each add the same rows.
	lock, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("another copy is running in %s", dir)
	}
	id, vm, err := audit.Fetch(root)
	if err != nil {
		return err
	}
	path := audit.Path(dir, id)
	copied, err := audit.Load(path)
	if err != nil {
		return err
	}
	fresh, err := audit.Check(copied, vm)
	if err != nil {
		return fmt.Errorf("%v (copy %s)", err, path)
	}
	all := append(copied, fresh...)
	if len(fresh) > 0 {
		if err := audit.Save(path, all); err != nil {
			return err
		}
	}
	last := approval.Genesis
	if len(all) > 0 {
		last = all[len(all)-1].SHA
	}
	fmt.Printf("ok audit copy: %d new rows, %d in all, last hash %s, in %s\n", len(fresh), len(all), last, path)
	return nil
}
