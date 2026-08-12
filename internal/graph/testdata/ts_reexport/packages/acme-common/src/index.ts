// Barrel: defines nothing, only re-exports — the shape that made cross-package
// calls invisible before #127 Phase 2.
export * from './String';                 // star re-export
export { encode as b64encode } from './Base64'; // named re-export with rename
export * as Arr from './Arr';             // namespace re-export
