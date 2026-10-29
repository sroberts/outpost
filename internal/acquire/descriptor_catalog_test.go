package acquire

import "testing"

// A publisher may describe the app and open paths, but not choose who gets in.
func TestParseDescriptor_CatalogFields(t *testing.T) {
	d, err := ParseDescriptor([]byte("title = \"Yarr\"\ndescription = \"RSS reader\"\npublic_paths = [\"/feed.xml\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if d.Title != "Yarr" || d.Description != "RSS reader" || len(d.PublicPaths) != 1 {
		t.Errorf("parsed %+v", d)
	}
	if _, err := ParseDescriptor([]byte("public_paths = [\"/*\"]\n")); err == nil {
		t.Error("a descriptor opening the whole app was accepted")
	}
}
