package dedupingchan

type Chan struct {
	In  chan<- interface{}
	Out <-chan interface{}
}

func New() *Chan {
	in := make(chan interface{})
	out := make(chan interface{})
	c := &Chan{
		In:  in,
		Out: out,
	}
	go c.watch(in, out)
	return c
}

func (c *Chan) watch(inChan <-chan interface{}, outChan chan<- interface{}) {
	defer close(outChan)
	var next interface{}
	var nextEmpty bool = true
	var closed bool
	m := make(map[interface{}]struct{})
	for {
		if nextEmpty {
			for k := range m {
				next = k
				nextEmpty = false
				delete(m, k)
				break
			}
		}
		if nextEmpty {
			if closed {
				return
			}
			var ok bool
			next, ok = <-inChan
			if !ok {
				closed = true
			} else {
				nextEmpty = false
			}
		} else if closed {
			outChan <- next
			nextEmpty = true
		} else {
			select {
			case in := <-inChan:
				m[in] = struct{}{}
			case outChan <- next:
				nextEmpty = true
			}
		}
	}
}

func (c *Chan) Close() {
	close(c.In)
}
