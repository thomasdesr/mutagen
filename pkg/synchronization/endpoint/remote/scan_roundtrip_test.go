package remote

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/mutagen-io/mutagen/pkg/logging"
	"github.com/mutagen-io/mutagen/pkg/synchronization"
	"github.com/mutagen-io/mutagen/pkg/synchronization/core"
)

// TestScanRoundTripDecodesSnapshot verifies the client/server transport well
// enough to serve as the harness for wiring tests: a real endpointClient talking
// to a real ServeEndpoint over an in-memory stream returns a snapshot of the
// server's synchronization root.
func TestScanRoundTripDecodesSnapshot(t *testing.T) {
	isolatedDataDirectory(t)
	root := populatedRoot(t)

	client := connectedEndpoint(t, root, "session-round-trip")

	snapshot, err, _ := client.Scan(context.Background(), nil, true)
	if err != nil {
		t.Fatal("unable to scan:", err)
	} else if snapshot.Content == nil {
		t.Fatal("scan returned no content")
	}

	if snapshot.Content.Kind != core.EntryKind_Directory {
		t.Error("root is not a directory:", snapshot.Content.Kind)
	}
	if _, ok := snapshot.Content.Contents["alpha.txt"]; !ok {
		t.Error("root does not contain alpha.txt")
	}
	if directory, ok := snapshot.Content.Contents["nested"]; !ok {
		t.Error("root does not contain nested")
	} else if _, ok := directory.Contents["beta.txt"]; !ok {
		t.Error("nested does not contain beta.txt")
	}
}

// connectedEndpoint returns a remote endpoint client for the specified root,
// served by a ServeEndpoint goroutine over an in-memory stream. Both the client
// and the server are torn down when the test completes. The calling test must
// have called isolatedDataDirectory, because the server side creates a scan cache
// and a staging root in the Mutagen data directory.
func connectedEndpoint(t *testing.T, root, session string) synchronization.Endpoint {
	t.Helper()

	clientStream, serverStream := net.Pipe()

	served := make(chan error, 1)
	go func() {
		served <- ServeEndpoint(logging.NewLogger(logging.LevelError, os.Stderr), serverStream)
	}()

	client, err := NewEndpoint(
		logging.NewLogger(logging.LevelError, os.Stderr),
		clientStream,
		root,
		session,
		synchronization.Version_Version1,
		&synchronization.Configuration{
			// Watching would race the test's own view of the root and isn't
			// needed to exercise scanning.
			WatchMode: synchronization.WatchMode_WatchModeNoWatch,
		},
		true,
	)
	if err != nil {
		serverStream.Close()
		<-served
		t.Fatal("unable to create endpoint client:", err)
	}

	t.Cleanup(func() {
		client.Shutdown()
		<-served
	})

	return client
}

// isolatedDataDirectory redirects endpoint state (scan caches, staging roots)
// into a directory owned by the test, so that tests neither read nor write the
// user's real Mutagen data directory. Sessions in one test share it, as they do
// in the daemon.
func isolatedDataDirectory(t *testing.T) {
	t.Helper()
	t.Setenv("MUTAGEN_DATA_DIRECTORY", t.TempDir())
}

// populatedRoot creates a temporary synchronization root with a small directory
// hierarchy and returns its path.
func populatedRoot(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "alpha.txt"), []byte("alpha contents"), 0600); err != nil {
		t.Fatal("unable to write file:", err)
	}
	nested := filepath.Join(root, "nested")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal("unable to create directory:", err)
	}
	if err := os.WriteFile(filepath.Join(nested, "beta.txt"), []byte("beta contents"), 0600); err != nil {
		t.Fatal("unable to write file:", err)
	}

	return root
}
