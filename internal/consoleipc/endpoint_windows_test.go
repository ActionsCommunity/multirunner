//go:build windows

package consoleipc

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/GerardSmit/multirunner/internal/securefile"
)

func TestBoundOperatorMarkerMustMatchIndependentBinding(t *testing.T) {
	const operator = "S-1-5-21-1-2-3-1001"
	for _, test := range []struct {
		name, marker string
		readErr      error
		wantOK       bool
	}{
		{"original operator", operator, nil, true},
		{"different operator", "S-1-5-21-1-2-3-1002", nil, false},
		{"LocalSystem marker", "S-1-5-18", nil, false},
		{"untrusted marker", "", securefile.ErrInsecurePermissions, false},
		{"missing marker", "", windows.ERROR_FILE_NOT_FOUND, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			actual, err := readBoundOperator(`C:\operator\install\history.db`, operator, func(path string) (*windows.SID, error) {
				if path != filepath.Join(`C:\operator\install`, "console.operator") {
					t.Fatalf("wrong operator path %q", path)
				}
				if test.readErr != nil {
					return nil, test.readErr
				}
				return windows.StringToSid(test.marker)
			})
			if (err == nil) != test.wantOK {
				t.Fatalf("marker validation = %v, want success %v", err, test.wantOK)
			}
			if test.wantOK && actual.String() != operator {
				t.Fatalf("pipe operator = %s, want bound operator %s", actual.String(), operator)
			}
			if test.readErr != nil && !errors.Is(err, test.readErr) {
				t.Fatalf("read error lost: %v", err)
			}
		})
	}
}

func TestServiceBootstrapAuthenticatesExplicitTokenDirectoriesBeforeOAuth(t *testing.T) {
	const database = `C:\console\history.db`
	const operator = "S-1-5-21-1-2-3-1001"
	const token = `C:\external-auth\github-user-token.json`
	for _, test := range []struct {
		name       string
		paths      []string
		bound      string
		tokenErr   error
		markerErr  error
		wantEvents []string
		wantErr    bool
	}{
		{"explicit external token", []string{token}, operator, nil, nil,
			[]string{"database binding", "database marker", "configured token binding", "OAuth read"}, false},
		{"no implicit token path", nil, operator, nil, nil,
			[]string{"database binding", "database marker", "OAuth read"}, false},
		{"ordinary foreground unchanged", []string{token}, "", nil, nil,
			[]string{"database binding", "OAuth read"}, false},
		{"untrusted marker stops token bootstrap", []string{token}, operator, nil, securefile.ErrInsecurePermissions,
			[]string{"database binding", "database marker"}, true},
		{"untrusted token stops authentication", []string{token, `C:\not-reached\token.json`}, operator, securefile.ErrInsecurePermissions, nil,
			[]string{"database binding", "database marker", "configured token binding"}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var events []string
			err := bootstrapServiceIdentity(database, test.paths,
				func(directory string) (string, error) {
					events = append(events, "database binding")
					if directory != filepath.Dir(database) {
						t.Fatalf("wrong database binding directory %q", directory)
					}
					return test.bound, nil
				},
				func(path string) (*windows.SID, error) {
					events = append(events, "database marker")
					if path != operatorIdentityPath(database) {
						t.Fatalf("wrong operator marker %q", path)
					}
					if test.markerErr != nil {
						return nil, test.markerErr
					}
					return windows.StringToSid(operator)
				},
				func(path, boundOperator string) error {
					events = append(events, "configured token binding")
					if path != token || boundOperator != operator {
						t.Fatalf("token bootstrap = %q, %q", path, boundOperator)
					}
					return test.tokenErr
				})
			if (err != nil) != test.wantErr {
				t.Fatalf("bootstrap error = %v, want error %v", err, test.wantErr)
			}
			if err == nil {
				events = append(events, "OAuth read")
			}
			if !reflect.DeepEqual(events, test.wantEvents) {
				t.Fatalf("bootstrap/auth ordering = %q, want %q", events, test.wantEvents)
			}
		})
	}
}
