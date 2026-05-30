package server

// maxInlineTargets is the number of peer addresses we can hold on the stack
// without heap allocation. For rooms ≤ 32 players this covers the common case.
const maxInlineTargets = 32
