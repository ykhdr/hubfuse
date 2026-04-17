package agent

import (
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsTLSCertError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "UnknownAuthorityError",
			err:  x509.UnknownAuthorityError{},
			want: true,
		},
		{
			name: "CertificateInvalidError",
			err:  x509.CertificateInvalidError{Reason: x509.Expired},
			want: true,
		},
		{
			name: "wrapped_UnknownAuthority",
			err:  fmt.Errorf("dial: %w", x509.UnknownAuthorityError{}),
			want: true,
		},
		{
			name: "net_OpError",
			err:  &net.OpError{Op: "dial", Err: errors.New("connection refused")},
			want: false,
		},
		{
			name: "generic_error",
			err:  errors.New("something went wrong"),
			want: false,
		},
		{
			name: "nil",
			err:  nil,
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isTLSCertError(tc.err)
			assert.Equal(t, tc.want, got, "isTLSCertError(%v)", tc.err)
		})
	}
}
