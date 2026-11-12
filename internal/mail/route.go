package mail

import (
	"crypto/rand"
	"errors"
	"math/big"
	"net/mail"
	"strings"
)

// aliasAlphabet leaves out letters and digits that read alike (0/o, 1/l/i),
// since an address is often typed from a screen.
const aliasAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"

// aliasLen is how long a random route is.
const aliasLen = 4

// newAlias makes a random route with at least one digit, so it can't be
// mistaken for, or collide with, an app's name made of words.
func newAlias() (string, error) {
	for {
		b := make([]byte, aliasLen)
		digit := false
		for i := range b {
			n, err := rand.Int(rand.Reader, big.NewInt(int64(len(aliasAlphabet))))
			if err != nil {
				return "", err
			}
			b[i] = aliasAlphabet[n.Int64()]
			digit = digit || (b[i] >= '0' && b[i] <= '9')
		}
		if digit {
			return string(b), nil
		}
	}
}

// account is the mailbox's own address, split for matching plus addresses.
type account struct {
	local, domain string
}

func parseAccount(address string) (account, error) {
	a, err := mail.ParseAddress(address)
	if err != nil || a.Name != "" || a.Address != address {
		return account{}, errors.New("want an address like you@gmail.com")
	}
	local, domain, _ := strings.Cut(a.Address, "@")
	if strings.Contains(local, "+") {
		return account{}, errors.New("use the account's own address, without a + part; Outpost adds those")
	}
	return account{local: local, domain: strings.ToLower(domain)}, nil
}

// gmail reports whether the domain is Gmail's, where dots in the local part
// don't count and googlemail.com is the same mailbox.
func gmail(domain string) bool { return domain == "gmail.com" || domain == "googlemail.com" }

func (a account) canonLocal(local string) string {
	local = strings.ToLower(local)
	if gmail(a.domain) {
		local = strings.ReplaceAll(local, ".", "")
	}
	return local
}

// route is the plus part of recipient, if it is this account's address.
func (a account) route(recipient string) (string, bool) {
	local, domain, ok := strings.Cut(strings.TrimSpace(recipient), "@")
	if !ok {
		return "", false
	}
	domain = strings.ToLower(domain)
	if domain != a.domain && (!gmail(domain) || !gmail(a.domain)) {
		return "", false
	}
	base, plus, ok := strings.Cut(local, "+")
	if !ok || plus == "" || a.canonLocal(base) != a.canonLocal(a.local) {
		return "", false
	}
	return strings.ToLower(plus), true
}

// address is the plus address for route.
func (a account) address(route string) string {
	return a.local + "+" + route + "@" + a.domain
}

// validRoute reports whether r can be a route: what an app's name or an
// alias is made of, and safe in an address.
func validRoute(r string) bool {
	if r == "" || len(r) > 64 {
		return false
	}
	for _, c := range r {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '_' && c != '.' {
			return false
		}
	}
	return true
}
