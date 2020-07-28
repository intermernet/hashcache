package hashcache

import (
	"encoding/binary"
	"fmt"
	"sync"
	"time"

	"github.com/dchest/siphash"
)

const (
	hashLen     = 64 // SipHash length
	bitsPerNode = 4  // Can be 4, 8 or 16. Needs benchmarking.
)

type node struct {
	parent   *node
	children [1 << bitsPerNode]*node
}

type endNode struct {
	tail    *node
	created uint64
	value   *[]byte
}

// Cache is a hash tree of keys which have been hashed using SipHash.
// It stores pointers to the values associated with the keys.
// It supports customisable key Time To Live and scavenge time.
type Cache struct {
	hkey0        uint64
	hkey1        uint64
	head         *node
	tails        map[*node]*endNode
	ttl          uint64 // milliseconds
	scavengeTime uint64 // milliseconds
	timer        *time.Timer
	mu           *sync.RWMutex
}

// NewCache will return a pointer to a newly instantiated Cache.
// It requires a 128 bit hash key in string form to initialise.
// If the key is longer or shorter than 128 bits it will be truncated
// or padded respectively.
// The cache TTL and scavenge time are set to 10 seconds and 1 second
// respectively. These values can be changed at any time by calling
// the SetTTL and SetScavengeTime methods.
func NewCache(hashKey string) *Cache {
	hKeyBytes := []byte(hashKey)
	hKeyLen := len(hKeyBytes)
	if hKeyLen < 16 {
		hKeyBytes = append(make([]byte, 16-hKeyLen), hKeyBytes...) // Pad hash key value
	}
	if hKeyLen > 16 {
		hKeyBytes = hKeyBytes[len(hKeyBytes)-16:] // Truncate hash key value
	}
	c := &Cache{
		hkey0: binary.LittleEndian.Uint64(hKeyBytes[:8]),
		hkey1: binary.LittleEndian.Uint64(hKeyBytes[8:]),
		head: &node{
			parent:   nil,
			children: [1 << bitsPerNode]*node{},
		},
		tails:        map[*node]*endNode{},
		ttl:          10000,
		scavengeTime: 1000,
		mu:           &sync.RWMutex{},
	}
	c.timer = time.NewTimer(time.Duration(c.scavengeTime) * time.Millisecond)
	go c.scavenge()
	return c
}

// Write will add the key and value to the cache.
// It will overwrite the key if it already exists.
func (c *Cache) Write(key, value []byte) {
	hash := c.hash(key)
	currentNode := c.head
	for i := 0; i < hashLen/bitsPerNode; i++ {
		currentByte := hash & (hashLen/bitsPerNode - 1)
		if currentNode.children[currentByte] == nil {
			c.mu.Lock()
			currentNode.children[currentByte] = &node{
				parent:   currentNode,
				children: [1 << bitsPerNode]*node{},
			}
			c.mu.Unlock()
		}
		currentNode = currentNode.children[currentByte]
		hash = hash >> bitsPerNode
	}
	c.mu.Lock()
	c.tails[currentNode] = &endNode{currentNode, uint64(time.Now().UnixNano()), &value}
	c.mu.Unlock()
}

// Read will try to read the value of a given key from the cache.
// It will return the data as []byte and true if the key is found,
// otherwise it will return false if the key isn't found.
func (c *Cache) Read(key []byte) ([]byte, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	hash := c.hash(key)
	currentNode := c.head
	for i := 0; i < hashLen/bitsPerNode; i++ {
		currentByte := hash & (hashLen/bitsPerNode - 1)
		if currentNode.children[currentByte] == nil {
			return nil, false
		}
		currentNode = currentNode.children[currentByte]
		hash = hash >> bitsPerNode
	}
	return *c.tails[currentNode].value, true
}

// Delete will remove an entry from the cache.
func (c *Cache) Delete(key []byte) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	hash := c.hash(key)
	currentNode := c.head
	for i := 0; i < hashLen/bitsPerNode; i++ {
		currentByte := hash & (hashLen/bitsPerNode - 1)
		if currentNode.children[currentByte] == nil {
			return false
		}
		currentNode = currentNode.children[currentByte]
		hash = hash >> bitsPerNode
	}
	delete(c.tails, currentNode)
	c.deleteNode(currentNode)
	return true
}

// Count returns the number of keys in the cache.
func (c *Cache) Count() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.tails)
}

// SetScavengeTime sets the frequency (in milliseconds) that the cache will check
// for entries that are oldet than their TTL.
// It must be greater than 0 milliseconds, and less than or equal to the cache TTL.
func (c *Cache) SetScavengeTime(st uint64) error {
	if st == 0 {
		return fmt.Errorf("scavenge time must be greater than 0 milliseconds")
	}
	if st > c.ttl {
		return fmt.Errorf("scavenge time must be less than or equal to cache TTL")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.scavengeTime = st
	c.timer.Reset(time.Duration(c.scavengeTime) * time.Millisecond)
	return nil
}

// SetTTL Sets the Time-To-Live value for cache entries.
// It must be greater than or equal to the scavenge time for the cache.
func (c *Cache) SetTTL(ttl uint64) error {
	if ttl < c.scavengeTime {
		return fmt.Errorf("TTL must be greater than or equal to cache scavenge time")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ttl = ttl
	return nil
}

func (c *Cache) hash(data []byte) uint64 {
	return siphash.Hash(c.hkey0, c.hkey1, data)
}

func checkParent(n *node) bool {
	count := 0
	for _, c := range n.parent.children {
		if c != nil {
			count++
		}
		if count > 1 {
			return false
		}
	}
	return true
}

func (c *Cache) deleteNode(n *node) {
	for checkParent(n) {
		n = n.parent
		n.children = [1 << bitsPerNode]*node{}
	}
}

func (c *Cache) scavenge() {
	for {
		select {
		case <-c.timer.C:
			now := uint64(time.Now().UnixNano() / 1e6)
			c.mu.Lock()
			for n, e := range c.tails {
				if now > (e.created/1e6)+c.ttl {
					delete(c.tails, n)
					c.deleteNode(n)
				}
			}
			c.timer.Reset(time.Duration(c.scavengeTime) * time.Millisecond)
			c.mu.Unlock()
		}
	}
}