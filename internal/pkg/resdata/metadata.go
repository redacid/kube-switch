//go:generate fyne bundle -o metadata_bundled.go -package resdata -name resourceAuthors ../../AUTHORS

package resdata

var Authors = resourceAuthors
