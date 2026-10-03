package s3

// ConditionalWrites puts b on the path it takes against AWS itself, If-None-Match on every
// write, while it points at a fake store's endpoint.
func ConditionalWrites(b *Backend) { b.conditionalWrite = true }
