// Package audit is what the off-VM copier and its test share: reading the VM's audit log, the copy's
// file, and the checks between them. See ADR 0024.
package audit

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"airgapai/gateway/approval"
)

// Row is one audit row, as copied. The time is in microseconds since the epoch, as PostgreSQL keeps it.
type Row struct {
	Seq      int64  `json:"seq"`
	AtMicros int64  `json:"at_us"`
	Actor    string `json:"actor"`
	Action   string `json:"action"`
	Decision string `json:"decision"`
	Detail   string `json:"detail"`
	Prev     string `json:"prev"`
	SHA      string `json:"sha"`
}

// Hash is the row's hash as the approval core computes it.
func (r Row) Hash() string {
	return approval.RowHash(r.Prev, r.Seq, time.UnixMicro(r.AtMicros).UTC(), r.Actor, r.Action, r.Decision, r.Detail)
}

// Lock is the approval core's audit lock, which writers of the log hold.
const Lock = 7_100_002

// PSQL runs SQL on the gateway database in the VM, as the database superuser, the only login the host
// can use without adding a password to the cluster, and returns what it printed.
func PSQL(root, sql string) (string, error) {
	cmd := exec.Command(filepath.Join(root, "infra/vm-ssh.sh"), `sudo /var/lib/rancher/rke2/bin/kubectl --kubeconfig /etc/rancher/rke2/rke2.yaml `+
		`-n retrieval exec -i deployment/postgres -- sh -c 'PGPASSWORD="$POSTGRES_PASSWORD" psql -X -h 127.0.0.1 -U postgres -d gateway -v ON_ERROR_STOP=1 -qAt'`)
	cmd.Stdin = strings.NewReader(sql)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("the VM's database: %v: %s", err, strings.TrimSpace(errOut.String()))
	}
	return out.String(), nil
}

// readSQL reads the database's identity and every row, in a read-only transaction.
const readSQL = `BEGIN READ ONLY;
SELECT system_identifier FROM pg_control_system();
SELECT json_build_object('seq', seq, 'at_us', (extract(epoch FROM at) * 1000000)::bigint, 'actor', actor,
  'action', action, 'decision', decision, 'detail', detail, 'prev', prev_sha256, 'sha', sha256)
  FROM gateway.audit ORDER BY seq;
COMMIT;
`

// Fetch reads the database's identity and every audit row from the VM.
func Fetch(root string) (string, []Row, error) {
	out, err := PSQL(root, readSQL)
	if err != nil {
		return "", nil, err
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	id := lines[0]
	if _, err := strconv.ParseUint(id, 10, 64); err != nil {
		return "", nil, fmt.Errorf("unexpected system identifier %q", id)
	}
	var rows []Row
	for _, l := range lines[1:] {
		var r Row
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			return "", nil, fmt.Errorf("unexpected row %q: %v", l, err)
		}
		rows = append(rows, r)
	}
	return id, rows, nil
}

// Path is the copy's file for one database: every new database has a new system identifier.
func Path(dir, id string) string { return filepath.Join(dir, "gateway-audit-"+id+".jsonl") }

// Load reads a copy, if there is one. Lines have no length limit.
func Load(path string) ([]Row, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var rows []Row
	rd := bufio.NewReader(f)
	for {
		line, err := rd.ReadBytes('\n')
		if len(line) > 0 {
			var r Row
			if err := json.Unmarshal(line, &r); err != nil {
				return nil, fmt.Errorf("the copy %s is unreadable: %v", path, err)
			}
			rows = append(rows, r)
		}
		if errors.Is(err, io.EOF) {
			return rows, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// Save replaces the copy with rows, atomically: a new file is written and synced beside it, then
// renamed over it, so a run that stops halfway leaves the old copy whole.
func Save(path string, rows []Row) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".copy-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	w := bufio.NewWriter(tmp)
	for _, r := range rows {
		raw, err := json.Marshal(r)
		if err != nil {
			tmp.Close()
			return err
		}
		w.Write(raw)
		w.WriteByte('\n')
	}
	if err := w.Flush(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Check compares the VM's rows with the copy and checks the new ones. It returns the new rows, or why
// the VM's log cannot be trusted: a copied row changed or gone, or a new row that does not chain.
func Check(copied, vm []Row) ([]Row, error) {
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
		case r.Hash() != r.SHA:
			return nil, fmt.Errorf("row %d's hash does not match its fields", r.Seq)
		}
		prev, seq = r.SHA, seq+1
	}
	return fresh, nil
}

// Literal is a SQL string literal.
func Literal(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
