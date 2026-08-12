// Mutual star re-export with loop_b — exercises resolveExport cycle termination.
// Neither defines `ghost`, so a lookup must traverse a→b→a(visited)→stop.
export * from './loop_b';
