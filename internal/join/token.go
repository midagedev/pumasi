package join

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// An invite token lets a host join: whoever shows it gets that repository's
// jobs, secrets included. So a token is made per invite, ends after an hour
// and admits one host unless the invite says otherwise. The controller keeps
// only its hash, one file per token: `pumasi invite` (another process) adds
// files, the running controller spends them.
type invite struct {
	Expires time.Time `json:"expires"`
	Uses    int       `json:"uses"`           // hosts it still admits
	Name    string    `json:"name,omitempty"` // the one name it admits ("" = any that is free)
}

const (
	InviteTTL  = time.Hour
	InviteUses = 1
)

func tokenDir(dir string) string { return filepath.Join(dir, "invites") }
func tokenFile(dir, token string) string {
	return filepath.Join(tokenDir(dir), hashSecret(token)+".json")
}
func (i invite) spent(now time.Time) bool { return i.Uses <= 0 || !now.Before(i.Expires) }
func writeInvite(path string, i invite) error {
	b, _ := json.Marshal(i)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// NewToken makes a token that admits uses hosts within ttl. With a name it
// admits only a host of that name: the controller vouching for a host of its
// own config.
func NewToken(dir, name string, uses int, ttl time.Duration) (string, error) {
	if uses < 1 || ttl <= 0 {
		return "", errors.New("a token needs at least one use and a time to live")
	}
	if err := os.MkdirAll(tokenDir(dir), 0o700); err != nil {
		return "", err
	}
	pruneTokens(dir, time.Now())
	t := randomHex(16)
	return t, writeInvite(tokenFile(dir, t), invite{Expires: time.Now().Add(ttl), Uses: uses, Name: name})
}

// pruneTokens removes the tokens that admit nobody any more.
func pruneTokens(dir string, now time.Time) {
	files, _ := filepath.Glob(filepath.Join(tokenDir(dir), "*.json"))
	for _, f := range files {
		var i invite
		if b, err := os.ReadFile(f); err != nil || json.Unmarshal(b, &i) != nil || i.spent(now) {
			_ = os.Remove(f)
		}
	}
}

// checkToken reports whether token admits a host of that name right now, the
// name it is bound to, and otherwise why not. With spend it uses the token up.
func checkToken(dir, token, name string, spend bool, now time.Time) (ok bool, bound string, why string) {
	if token == "" {
		return false, "", ""
	}
	path := tokenFile(dir, token)
	var i invite
	b, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(b, &i) != nil {
		return false, "", "the controller does not know this token (a token admits one host unless `pumasi invite --uses N` said more)"
	}
	switch {
	case !now.Before(i.Expires):
		return false, "", fmt.Sprintf("the token ended at %s", i.Expires.Local().Format("01-02 15:04"))
	case i.Uses <= 0:
		return false, "", "the token has been used"
	case i.Name != "" && i.Name != name:
		return false, "", fmt.Sprintf("the token is for the host named %s", i.Name)
	}
	if spend {
		i.Uses--
		if i.Uses <= 0 {
			err = os.Remove(path)
		} else {
			err = writeInvite(path, i)
		}
		if err != nil {
			return false, "", "the controller could not record the token's use: " + err.Error()
		}
	}
	return true, i.Name, ""
}

// Advertised is the addresses hosts may dial for a listen address: the one of
// the default route first, then this machine's others. A listen address that
// names a host is that address alone.
func Advertised(listen string) []string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return []string{listen}
	}
	if ip := net.ParseIP(host); host != "" && (ip == nil || !ip.IsUnspecified()) {
		return []string{listen}
	}
	var all []string
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() {
				all = append(all, n.IP.String())
			}
		}
	}
	// The address of the default route; no packet is sent.
	first := "127.0.0.1"
	if c, err := net.Dial("udp4", "192.0.2.1:9"); err == nil {
		first = c.LocalAddr().(*net.UDPAddr).IP.String()
		c.Close()
	} else if len(all) > 0 {
		first = all[0]
	}
	out := []string{net.JoinHostPort(first, port)}
	for _, a := range all {
		if a != first {
			out = append(out, net.JoinHostPort(a, port))
		}
	}
	return out
}
