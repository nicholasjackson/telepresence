package cli

import (
	"fmt"
	"io/ioutil"
	"os"
	"strings"
	"testing"

	"gotest.tools/assert"
	"k8s.io/apimachinery/pkg/util/yaml"

	"github.com/datawire/ambassador/pkg/kates"
	"github.com/datawire/dlib/dlog"
)

// getLicenseString is a helper function for reading the
// license from a file and returning the string
// representation of that value
func getLicenseString(licenseFile string) (string, error) {
	license, err := ioutil.ReadFile(licenseFile)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(string(license), "\n"), nil
}

// getSecret is a helper function for reading the secret
// generated by getCloudLicense and returning it as a
// *kates.Secret
func getSecret(secretFile string) (*kates.Secret, error) {
	secretReader, err := os.Open(secretFile)
	if err != nil {
		return nil, err
	}
	defer secretReader.Close()
	sec := &kates.Secret{}
	sez := yaml.NewYAMLOrJSONDecoder(secretReader, 1000)

	if err := sez.Decode(&sec); err != nil {
		return nil, err
	}
	return sec, nil
}

func Test_createLicenseSecret(t *testing.T) {
	ctx := dlog.NewTestContext(t, false)
	stdout := dlog.StdLogger(ctx, dlog.LogLevelInfo).Writer()

	// Prepare a file for getCloudLicense to write the secret to
	tmpDir := t.TempDir()
	secretFile := fmt.Sprintf("%s/license", tmpDir)

	// File containing the license we are using for this test
	// Note: This is just a random jwt found at jwt.io, since we
	// are only testing the generation of a license secret here.
	licenseFile := "testdata/license"

	// Host domain, this is a made-up domain just to ensure that
	// we never hardcode the domain
	hostDomain := "test.datawire.io"

	if err := getCloudLicense(ctx, stdout, "", secretFile, licenseFile, hostDomain); err != nil {
		t.Fatal(err)
	}

	// Get the generated secret as a *kates.Secret
	secret, err := getSecret(secretFile)
	if err != nil {
		t.Fatal(err)
	}

	// Get the original license to verify it matches what's
	// in the secret
	license, err := getLicenseString(licenseFile)
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, string(secret.Data["hostDomain"]), hostDomain)
	assert.Equal(t, string(secret.Data["license"]), license)
}
