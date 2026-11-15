// Auditcopy keeps a copy of the gateway's audit log off the VM, on the host, and checks the VM's log
// against it. The hash chain in the VM shows that rows were not changed after the fact only if the
// last hash is kept somewhere the database cannot reach (ADR 0015). Here every row is kept, so a row
// rewritten after it was copied, with the whole chain recomputed so that it still verifies in the VM,
// no longer matches the copy. See ADR 0024.
//
// Usage: auditcopy -root REPO -dir DIR [copy | forge SEQ DETAIL | restore]
//
//	copy     the default: check every copied row is unchanged in the VM, check each new row's
//	         chain and hash, then append the new rows to the copy
//	forge    for tests, print the SQL an attacker with the database superuser would run to rewrite
//	         row SEQ's detail and recompute every hash from there, so the VM's chain still verifies
//	restore  for tests, print the SQL that puts every copied row back as the copy holds it
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"airgapai/gateway/approval"
)

// row is one audit row, as copied. The time is in microseconds since the epoch, as PostgreSQL keeps it.
type row struct {
	Seq      int64  `json:"seq"`
	AtMicros int64  `json:"at_us"`
	Actor    string `json:"actor"`
	Action   string `json:"action"`
	Decision string `json:"decision"`
	Detail   string `json:"detail"`
	Prev     string `json:"prev"`
	SHA      string `json:"sha"`
}

func (r row) hash() string {
	return approval.RowHash(r.Prev, r.Seq, time.UnixMicro(r.AtMicros).UTC(), r.Actor, r.Action, r.Decision, r.Detail)
}

// The read runs in a read-only transaction, as the database superuser: the only login the host can
// use without adding a password to the cluster, and one the copier never writes with.
const readSQL = `BEGIN READ ONLY;
SELECT system_identifier FROM pg_control_system();
SELECT json_build_object('seq', seq, 'at_us', (extract(epoch FROM at) * 1000000)::bigint, 'actor', actor,
  'action', action, 'decision', decision, 'detail', detail, 'prev', prev_sha256, 'sha', sha256)
  FROM gateway.audit ORDER BY seq;
COMMIT;
`

// psql runs SQL on the gateway database in the VM, as the superuser, and returns its output.
func psql(root, sql string) (string, error) {
	cmd := exec.Command(filepath.Join(root, "infra/vm-ssh.sh"), `sudo /var/lib/rancher/rke2/bin/kubectl --kubeconfig /etc/rancher/rke2/rke2.yaml `+
		`-n retrieval exec -i deployment/postgres -- sh -c 'PGPASSWORD="$POSTGRES_PASSWORD" psql -h 127.0.0.1 -U postgres -d gateway -v ON_ERROR_STOP=1 -qAt'`)
	cmd.Stdin = strings.NewReader(sql)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("reading the VM's audit log: %v: %s", err, strings.TrimSpace(errOut.String()))
	}
	return out.String(), nil
}

// fetch reads the database's identity and every audit row from the VM.
func fetch(root string) (string, []row, error) {
	out, err := psql(root, readSQL)
	if err != nil {
		return "", nil, err
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) == 0 || lines[0] == "" {
		return "", nil, errors.New("no system identifier in the answer")
	}
	id := lines[0]
	if _, err := strconv.ParseUint(id, 10, 64); err != nil {
		return "", nil, fmt.Errorf("unexpected system identifier %q", id)
	}
	var rows []row
	for _, l := range lines[1:] {
		var r row
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			return "", nil, fmt.Errorf("unexpected row %q: %v", l, err)
		}
		rows = append(rows, r)
	}
	return id, rows, nil
}

// load reads the copy for one database, if there is one.
func load(path string) ([]row, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var rows []row
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var r row
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			return nil, fmt.Errorf("the copy %s is unreadable: %v", path, err)
		}
		rows = append(rows, r)
	}
	return rows, sc.Err()
}

// check compares the VM's rows with the copy and checks the new ones. It returns the new rows, or why
// the VM's log cannot be trusted: a copied row changed or gone, or a new row that does not chain.
func check(copied, vm []row) ([]row, error) {
	if len(vm) < len(copied) {
		return nil, fmt.Errorf("the VM's log has %d rows, but %d were copied: rows were removed", len(vm), len(copied))
	}
	for i, c := range copied {
		if vm[i] != c {
			return nil, fmt.Errorf("row %d in the VM differs from the copy: it was changed after it was copied", c.Seq)
		}
	}
	prev, seq := approval.Genesis, int64(1)
	if len(copied) > 0 {
		last := copied[len(copied)-1]
		prev, seq = last.SHA, last.Seq+1
	}
	fresh := vm[len(copied):]
	for _, r := range fresh {
		switch {
		case r.Seq != seq:
			return nil, fmt.Errorf("row %d follows row %d", r.Seq, seq-1)
		case r.Prev != prev:
			return nil, fmt.Errorf("row %d does not chain to the row before it", r.Seq)
		case r.hash() != r.SHA:
			return nil, fmt.Errorf("row %d's hash does not match its fields", r.Seq)
		}
		prev, seq = r.SHA, seq+1
	}
	return fresh, nil
}

func appendRows(path string, rows []row) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	for _, r := range rows {
		raw, err := json.Marshal(r)
		if err != nil {
			f.Close()
			return err
		}
		w.Write(raw)
		w.WriteByte('\n')
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// literal is a SQL string literal.
func literal(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// updates is the SQL that sets rows to these values.
func updates(rows []row) string {
	var b strings.Builder
	b.WriteString("BEGIN;\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "UPDATE gateway.audit SET detail = %s, prev_sha256 = %s, sha256 = %s WHERE seq = %d;\n",
			literal(r.Detail), literal(r.Prev), literal(r.SHA), r.Seq)
	}
	b.WriteString("COMMIT;\n")
	return b.String()
}

func main() {
	root := flag.String("root", ".", "the repository")
	dir := flag.String("dir", "", "where the copies are kept, one file per database")
	flag.Parse()
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "-dir is required")
		os.Exit(2)
	}
	if err := run(*root, *dir, flag.Args()); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL", err)
		os.Exit(1)
	}
}

func run(root, dir string, args []string) error {
	id, vm, err := fetch(root)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, "gateway-audit-"+id+".jsonl")
	copied, err := load(path)
	if err != nil {
		return err
	}
	cmd := "copy"
	if len(args) > 0 {
		cmd = args[0]
	}
	switch cmd {
	case "copy":
		fresh, err := check(copied, vm)
		if err != nil {
			return fmt.Errorf("%v (copy %s)", err, path)
		}
		if err := appendRows(path, fresh); err != nil {
			return err
		}
		last := approval.Genesis
		if all := append(copied, fresh...); len(all) > 0 {
			last = all[len(all)-1].SHA
		}
		fmt.Printf("ok audit copy: %d new rows, %d in all, last hash %s, in %s\n", len(fresh), len(copied)+len(fresh), last, path)
		return nil
	case "forge":
		if len(args) != 3 {
			return errors.New("usage: forge SEQ DETAIL")
		}
		seq, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil || seq < 1 || seq > int64(len(vm)) {
			return fmt.Errorf("no row %s", args[1])
		}
		detail, err := approval.Canonical([]byte(args[2]))
		if err != nil {
			return err
		}
		forged := append([]row(nil), vm...)
		forged[seq-1].Detail = detail
		prev := approval.Genesis
		if seq > 1 {
			prev = forged[seq-2].SHA
		}
		for i := seq - 1; i < int64(len(forged)); i++ {
			forged[i].Prev = prev
			forged[i].SHA = forged[i].hash()
			prev = forged[i].SHA
		}
		fmt.Print(updates(forged[seq-1:]))
		return nil
	case "restore":
		fmt.Print(updates(copied))
		return nil
	}
	return fmt.Errorf("unknown command %q", cmd)
}
