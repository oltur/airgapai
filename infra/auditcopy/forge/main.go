// Forge is for test/audit-copy.sh only. It does to the VM's audit log what an attacker with the
// database superuser could, and undoes it from the off-VM copy. It is a separate program, so the
// copier itself only ever reads. See ADR 0024.
//
// Usage: forge -root REPO -dir DIR rewrite SEQ DETAIL | restore
//
//	rewrite  set row SEQ's detail, and recompute every hash from there, so the VM's chain verifies
//	restore  put every copied row back as the copy holds it, and re-link any row added since
//
// Each holds the audit lock, as the gateway does when it writes, and refuses if the log changed after
// it was read, so it cannot race a row being appended.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"airgapai/auditcopy/audit"
	"airgapai/gateway/approval"
)

func main() {
	root := flag.String("root", ".", "the repository")
	dir := flag.String("dir", "", "where the copies are kept")
	flag.Parse()
	if err := run(*root, *dir, flag.Args()); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL", err)
		os.Exit(1)
	}
}

// rechain recomputes the link and hash of rows[i:], in place.
func rechain(rows []audit.Row, i int) {
	prev := approval.Genesis
	if i > 0 {
		prev = rows[i-1].SHA
	}
	for ; i < len(rows); i++ {
		rows[i].Prev = prev // first: the hash covers it
		rows[i].SHA = rows[i].Hash()
		prev = rows[i].SHA
	}
}

// apply writes rows, under the audit lock, if the log still has exactly the rows it had when read.
func apply(root string, read, rows []audit.Row) error {
	last := ""
	if len(read) > 0 {
		last = read[len(read)-1].SHA
	}
	var b strings.Builder
	fmt.Fprintf(&b, "BEGIN;\nSELECT pg_advisory_xact_lock(%d);\n", audit.Lock)
	fmt.Fprintf(&b, `DO $$ BEGIN
  IF (SELECT count(*) FROM gateway.audit) <> %d OR coalesce((SELECT sha256 FROM gateway.audit ORDER BY seq DESC LIMIT 1), '') <> %s THEN
    RAISE EXCEPTION 'the audit log changed after it was read';
  END IF;
END $$;
`, len(read), audit.Literal(last))
	for _, r := range rows {
		fmt.Fprintf(&b, "UPDATE gateway.audit SET at = to_timestamp(%d / 1000000.0), actor = %s, action = %s, decision = %s, "+
			"detail = %s, prev_sha256 = %s, sha256 = %s WHERE seq = %d;\n",
			r.AtMicros, audit.Literal(r.Actor), audit.Literal(r.Action), audit.Literal(r.Decision),
			audit.Literal(r.Detail), audit.Literal(r.Prev), audit.Literal(r.SHA), r.Seq)
	}
	b.WriteString("COMMIT;\n")
	_, err := audit.PSQL(root, b.String())
	return err
}

func run(root, dir string, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: forge -root REPO -dir DIR rewrite SEQ DETAIL | restore")
	}
	id, vm, err := audit.Fetch(root)
	if err != nil {
		return err
	}
	switch args[0] {
	case "rewrite":
		if len(args) != 3 {
			return errors.New("usage: rewrite SEQ DETAIL")
		}
		seq, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil || seq < 1 || seq > int64(len(vm)) {
			return fmt.Errorf("no row %s", args[1])
		}
		detail, err := approval.Canonical([]byte(args[2]))
		if err != nil {
			return err
		}
		forged := append([]audit.Row(nil), vm...)
		forged[seq-1].Detail = detail
		rechain(forged, int(seq-1))
		return apply(root, vm, forged[seq-1:])
	case "restore":
		copied, err := audit.Load(audit.Path(dir, id))
		if err != nil {
			return err
		}
		if len(copied) > len(vm) {
			return fmt.Errorf("the copy has %d rows and the VM %d: restore does not add rows", len(copied), len(vm))
		}
		restored := append(append([]audit.Row(nil), copied...), vm[len(copied):]...)
		rechain(restored, len(copied)) // rows added since the copy chain on from it
		return apply(root, vm, restored)
	}
	return fmt.Errorf("unknown command %q", args[0])
}
