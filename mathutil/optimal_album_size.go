package mathutil

func OptimalAlbumSize(total int) int {
	const max = 10
	numAlbums := total / max // 10%1
	if total%max != 0 {
		numAlbums++
	}
	if total%numAlbums == 0 {
		return total / numAlbums
	}
	return total/numAlbums + 1
}
