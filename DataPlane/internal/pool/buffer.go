package pool

import "sync"

const BufferSize = 4096


var bufferPool = sync.Pool {
	 
	New : func() any {
		buffer := make([]byte, BufferSize)
		return &buffer
	},
}

func Get() *[]byte {
	return bufferPool.Get().(*[]byte)
}

func Put(b *[]byte) {
	bufferPool.Put(b)
}