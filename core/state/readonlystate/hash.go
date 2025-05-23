package readonlystate

import "github.com/ethereum/go-ethereum/common"

func fnv32Hash(key common.Hash) uint32 {
	hash := uint32(2166136261)
	const prime32 = uint32(16777619)
	for i := 0; i < common.HashLength; i++ {
		hash *= prime32
		hash ^= uint32(key[i])
	}
	return hash
}

func fnv32Address(key common.Address) uint32 {
	hash := uint32(2166136261)
	const prime32 = uint32(16777619)
	for i := 0; i < common.AddressLength; i++ {
		hash *= prime32
		hash ^= uint32(key[i])
	}
	return hash
}
