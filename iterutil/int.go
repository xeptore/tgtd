package iterutil

type IntIterator struct {
	i int
}

func Int(init int) IntIterator {
	return IntIterator{i: init}
}

func (i *IntIterator) Next() int {
	i.i++
	return i.i
}
