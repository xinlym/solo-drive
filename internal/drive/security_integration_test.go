package drive

import (
	"context"
	"net/http"
	"testing"
)

func TestFileDeletionCancelsAllShareStreams(t *testing.T) {
	a, server, client, csrf := newTestApp(t)
	file := fixtureFile(t, a, "test.bin", []byte("protected-content"))
	first := fixtureShare(t, client, server.URL, csrf, file.ID)
	second := fixtureShare(t, client, server.URL, csrf, file.ID)
	for _, id := range []string{first.ID, second.ID} {
		lease, rejection := a.downloads.Acquire(context.Background(), "peer", id, false)
		if rejection != nil {
			t.Fatal(rejection)
		}
		defer lease.Release()
		defer func() {
			select {
			case <-lease.Context().Done():
			default:
				t.Error("deleted file kept an active share stream")
			}
		}()
	}
	expectStatus(t, requestTest(t, client, "DELETE", server.URL+"/api/files/"+file.ID, csrf, ""), 200)
	expectStatus(t, requestTest(t, http.DefaultClient, "GET", server.URL+first.DownloadURL, "", ""), 404)
}
