package s3

// ConditionalWrites puts b on the path it takes against AWS itself, If-None-Match on every
// write, while it points at a fake store's endpoint.
func ConditionalWrites(b *Backend) { b.conditionalWrite = true }

// PartPlan is how an object of size bytes is cut into parts of at least smallest.
func PartPlan(size, smallest int64) (partSize int64, count int) {
	p := planParts(size, smallest)
	return p.partSize, p.count
}
