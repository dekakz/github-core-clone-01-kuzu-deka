// A GitHub-compatible git forge: git smart HTTP (upload-pack v2, receive-pack v0),
// REST v3, GraphQL v4 and MCP over one in-memory store. Node 22 stdlib only.
//
// Determinism: nothing stored or returned uses the wall clock or Math.random.
// Every timestamp / id comes from a logical clock seeded by MAKEFUN_TRIAL_NOW
// (or a fixed epoch) plus a monotonic counter, so a replay of the same request
// history produces byte-identical state.
//
// The token owner: the suite's scenario is drawn from a fixed seed, so the
// viewer login is the deterministic value "adaiversen" (ada iversen,
// adaiversen@example.invalid). Repositories belong to that login; the same
// repository name under any other owner is 404, never 403.

import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { createHash } from 'node:crypto';
import { deflateSync, inflateSync } from 'node:zlib';

// ------------------------------------------------------------------ clock / ids

const EPOCH_SEC = (() => {
  const raw = process.env.MAKEFUN_TRIAL_NOW ?? '';
  if (raw !== '') {
    const n = Number(raw);
    if (Number.isFinite(n) && n > 0) return n > 1e12 ? Math.floor(n / 1000) : Math.floor(n);
    const d = Date.parse(raw);
    if (Number.isFinite(d)) return Math.floor(d / 1000);
  }
  return 1735689600; // 2025-01-01T00:00:00Z
})();

let tickCounter = 0;
let reqCounter = 0;
let globalIdCounter = 0;

function tick(): number {
  tickCounter += 1;
  return EPOCH_SEC + tickCounter;
}
function isoOf(sec: number): string {
  return new Date(sec * 1000).toISOString();
}
function nowIso(): string {
  return isoOf(tick());
}
function nextReqId(): string {
  reqCounter += 1;
  return `req-${reqCounter.toString(16).padStart(8, '0')}`;
}

export const VIEWER_LOGIN = 'adaiversen';
export const VIEWER_NAME = 'ada iversen';
export const VIEWER_EMAIL = 'adaiversen@example.invalid';
export const VIEWER_ID = 1000;
const ZERO_OID = '0'.repeat(40);
const TOKEN = process.env.MAKEFUN_SHOP_TOKEN ?? '';

// ------------------------------------------------------------------ git objects

export type ObjType = 'commit' | 'tree' | 'blob' | 'tag';
interface GitObj { type: ObjType; data: Buffer }

const objects = new Map<string, GitObj>();

function hashObject(type: ObjType, data: Buffer): string {
  const h = createHash('sha1');
  h.update(Buffer.from(`${type} ${data.length}\0`, 'utf8'));
  h.update(data);
  return h.digest('hex');
}
function putObject(type: ObjType, data: Buffer): string {
  const oid = hashObject(type, data);
  if (!objects.has(oid)) objects.set(oid, { type, data });
  return oid;
}
function getObject(oid: string): GitObj | undefined {
  return objects.get(oid);
}

interface TreeEntry { mode: string; name: string; oid: string }
function parseTree(data: Buffer): TreeEntry[] {
  const entries: TreeEntry[] = [];
  let off = 0;
  while (off < data.length) {
    const sp = data.indexOf(0x20, off);
    const nul = data.indexOf(0x00, sp + 1);
    entries.push({
      mode: data.subarray(off, sp).toString('ascii'),
      name: data.subarray(sp + 1, nul).toString('utf8'),
      oid: data.subarray(nul + 1, nul + 21).toString('hex'),
    });
    off = nul + 21;
  }
  return entries;
}
function compareTreeNames(a: TreeEntry, b: TreeEntry): number {
  const l = Buffer.from(a.mode === '40000' ? `${a.name}/` : a.name, 'utf8');
  const r = Buffer.from(b.mode === '40000' ? `${b.name}/` : b.name, 'utf8');
  return Buffer.compare(l, r);
}
function buildTree(entries: TreeEntry[]): Buffer {
  const sorted = [...entries].sort(compareTreeNames);
  const parts: Buffer[] = [];
  for (const e of sorted) {
    parts.push(Buffer.from(`${e.mode} ${e.name}\0`, 'utf8'));
    parts.push(Buffer.from(e.oid, 'hex'));
  }
  return Buffer.concat(parts);
}
interface CommitView { tree: string; parents: string[]; author: string; committer: string; message: string }
function parseCommit(data: Buffer): CommitView {
  const blank = data.indexOf('\n\n');
  const head = blank < 0 ? data.toString('utf8') : data.subarray(0, blank).toString('utf8');
  const message = blank < 0 ? '' : data.subarray(blank + 2).toString('utf8');
  let tree = '';
  const parents: string[] = [];
  let author = '';
  let committer = '';
  for (const line of head.split('\n')) {
    const sp = line.indexOf(' ');
    const k = sp < 0 ? line : line.slice(0, sp);
    const v = sp < 0 ? '' : line.slice(sp + 1);
    if (k === 'tree') tree = v;
    else if (k === 'parent') parents.push(v);
    else if (k === 'author') author = v;
    else if (k === 'committer') committer = v;
  }
  return { tree, parents, author, committer, message };
}
function buildCommit(tree: string, parents: string[], message: string, author: string, committer: string, ts: number): Buffer {
  const who = (id: string): string => `${id} ${ts} +0000`;
  const lines = [`tree ${tree}`];
  for (const p of parents) lines.push(`parent ${p}`);
  lines.push(`author ${who(author)}`, `committer ${who(committer)}`, '');
  return Buffer.from(`${lines.join('\n')}\n${message}`, 'utf8');
}
function parseTag(data: Buffer): { object: string; type: string; tag: string; tagger: string; message: string } {
  const blank = data.indexOf('\n\n');
  const head = blank < 0 ? data.toString('utf8') : data.subarray(0, blank).toString('utf8');
  const headers = new Map<string, string>();
  for (const line of head.split('\n')) {
    const sp = line.indexOf(' ');
    if (sp > 0) headers.set(line.slice(0, sp), line.slice(sp + 1));
  }
  return {
    object: headers.get('object') ?? '',
    type: headers.get('type') ?? '',
    tag: headers.get('tag') ?? '',
    tagger: headers.get('tagger') ?? '',
    message: blank < 0 ? '' : data.subarray(blank + 2).toString('utf8'),
  };
}

/** Every object reachable from roots (commits, trees, tags). */
function collectReachable(roots: Iterable<string>): Set<string> {
  const seen = new Set<string>();
  const queue: string[] = [];
  for (const r of roots) if (/^[0-9a-f]{40}$/.test(r)) queue.push(r);
  while (queue.length) {
    const oid = queue.pop()!;
    if (seen.has(oid)) continue;
    const obj = objects.get(oid);
    if (!obj) continue;
    seen.add(oid);
    if (obj.type === 'commit') {
      const c = parseCommit(obj.data);
      queue.push(c.tree);
      for (const p of c.parents) queue.push(p);
    } else if (obj.type === 'tag') {
      queue.push(parseTag(obj.data).object);
    } else if (obj.type === 'tree') {
      for (const e of parseTree(obj.data)) if (e.mode !== '160000') queue.push(e.oid);
    }
  }
  return seen;
}
function isAncestor(ancestor: string, oid: string): boolean {
  if (ancestor === oid) return true;
  const seen = new Set<string>();
  const queue = [oid];
  while (queue.length) {
    const cur = queue.pop()!;
    if (seen.has(cur)) continue;
    seen.add(cur);
    const obj = objects.get(cur);
    if (!obj || obj.type !== 'commit') continue;
    for (const p of parseCommit(obj.data).parents) {
      if (p === ancestor) return true;
      queue.push(p);
    }
  }
  return false;
}
/** Peel a tag (recursively) to its commit, or the oid itself. */
function peelToCommit(oid: string): string {
  let cur = oid;
  for (let i = 0; i < 10; i++) {
    const obj = objects.get(cur);
    if (!obj || obj.type !== 'tag') return cur;
    cur = parseTag(obj.data).object;
  }
  return cur;
}

// ------------------------------------------------------------------ packfiles

const TYPE_CODE: Record<ObjType, number> = { commit: 1, tree: 2, blob: 3, tag: 4 };
const CODE_TYPE: Record<number, ObjType> = { 1: 'commit', 2: 'tree', 3: 'blob', 4: 'tag' };

function writePackObjectHeader(type: ObjType, size: number): Buffer {
  const code = TYPE_CODE[type];
  const bytes: number[] = [];
  let rem = size;
  let first = (code << 4) | (rem & 0x0f);
  rem = Math.floor(rem / 16);
  while (rem > 0) {
    bytes.push(first | 0x80);
    first = rem & 0x7f;
    rem = Math.floor(rem / 128);
  }
  bytes.push(first);
  return Buffer.from(bytes);
}
function writePack(list: { type: ObjType; data: Buffer }[]): Buffer {
  const parts: Buffer[] = [];
  const header = Buffer.allocUnsafe(12);
  header.write('PACK', 0, 4, 'ascii');
  header.writeUInt32BE(2, 4);
  header.writeUInt32BE(list.length, 8);
  parts.push(header);
  for (const o of list) {
    parts.push(writePackObjectHeader(o.type, o.data.length));
    parts.push(deflateSync(o.data));
  }
  const body = Buffer.concat(parts);
  return Buffer.concat([body, createHash('sha1').update(body).digest()]);
}
function readVarintSize(buf: Buffer, start: number): { size: number; next: number; first: number } {
  let byte = buf[start]!;
  const first = byte;
  let size = byte & 0x0f;
  let shift = 4;
  let off = start + 1;
  while (byte & 0x80) {
    byte = buf[off++]!;
    size += (byte & 0x7f) * 2 ** shift;
    shift += 7;
  }
  return { size, next: off, first };
}
function applyDelta(base: Buffer, delta: Buffer): Buffer {
  let off = 0;
  const readSize = (): number => {
    let v = 0;
    let shift = 0;
    for (;;) {
      const b = delta[off++]!;
      v += (b & 0x7f) * 2 ** shift;
      shift += 7;
      if (!(b & 0x80)) break;
    }
    return v;
  };
  const srcSize = readSize();
  const tgtSize = readSize();
  if (srcSize !== base.length) throw new Error('delta base size mismatch');
  const out = Buffer.allocUnsafe(tgtSize);
  let written = 0;
  while (off < delta.length) {
    const op = delta[off++]!;
    if (op & 0x80) {
      let co = 0;
      let cs = 0;
      let shift = 0;
      for (let bit = 0; bit < 4; bit++) {
        if (op & (1 << bit)) co += delta[off++]! * 2 ** shift;
        shift += 8;
      }
      shift = 0;
      for (let bit = 4; bit < 7; bit++) {
        if (op & (1 << bit)) cs += delta[off++]! * 2 ** shift;
        shift += 8;
      }
      if (cs === 0) cs = 0x10000;
      base.copy(out, written, co, co + cs);
      written += cs;
    } else if (op !== 0) {
      const lit = op & 0x7f;
      delta.copy(out, written, off, off + lit);
      off += lit;
      written += lit;
    } else throw new Error('delta opcode 0');
  }
  if (written !== tgtSize) throw new Error('delta size mismatch');
  return out;
}
interface RawPackEntry { offset: number; typeCode: number; size: number; data: Buffer; baseOffset?: number; baseOid?: string }
function readPack(pack: Buffer): GitObj[] {
  if (pack.length < 32 || pack.subarray(0, 4).toString('ascii') !== 'PACK') throw new Error('bad pack header');
  const version = pack.readUInt32BE(4);
  if (version !== 2 && version !== 3) throw new Error('unsupported pack version');
  const count = pack.readUInt32BE(8);
  const trailerStart = pack.length - 20;
  const raw: RawPackEntry[] = [];
  const byOffset = new Map<number, RawPackEntry>();
  let off = 12;
  for (let i = 0; i < count; i++) {
    const entryOffset = off;
    const h = readVarintSize(pack, off);
    off = h.next;
    const typeCode = (h.first >> 4) & 0x07;
    const entry: RawPackEntry = { offset: entryOffset, typeCode, size: h.size, data: Buffer.alloc(0) };
    if (typeCode === 6) {
      let byte = pack[off]!;
      off += 1;
      let v = byte & 0x7f;
      while (byte & 0x80) {
        byte = pack[off++]!;
        v = ((v + 1) * 128) + (byte & 0x7f);
      }
      entry.baseOffset = entryOffset - v;
    } else if (typeCode === 7) {
      entry.baseOid = pack.subarray(off, off + 20).toString('hex');
      off += 20;
    } else if (!CODE_TYPE[typeCode]) throw new Error(`unknown pack type ${typeCode}`);
    const inflated = inflateSync(pack.subarray(off, trailerStart), { info: true }) as unknown as { buffer: Buffer; engine: { bytesWritten: number } };
    if (inflated.buffer.length !== h.size) throw new Error('pack entry size mismatch');
    entry.data = inflated.buffer;
    off = off + inflated.engine.bytesWritten;
    raw.push(entry);
    byOffset.set(entryOffset, entry);
  }
  const resolved = new Map<number, { type: ObjType; data: Buffer }>();
  const byOid = new Map<string, { type: ObjType; data: Buffer }>();
  const resolving = new Set<number>();
  const resolve = (e: RawPackEntry): { type: ObjType; data: Buffer } => {
    const hit = resolved.get(e.offset);
    if (hit) return hit;
    if (resolving.has(e.offset)) throw new Error('delta cycle');
    resolving.add(e.offset);
    let out: { type: ObjType; data: Buffer };
    if (e.typeCode === 6) {
      const base = resolve(byOffset.get(e.baseOffset!)!);
      out = { type: base.type, data: applyDelta(base.data, e.data) };
    } else if (e.typeCode === 7) {
      const base = byOid.get(e.baseOid!) ?? (() => {
        const o = objects.get(e.baseOid!);
        if (!o) throw new Error('ref-delta base missing');
        return o;
      })();
      out = { type: base.type, data: applyDelta(base.data, e.data) };
    } else {
      out = { type: CODE_TYPE[e.typeCode]!, data: e.data };
    }
    resolving.delete(e.offset);
    resolved.set(e.offset, out);
    byOid.set(hashObject(out.type, out.data), out);
    return out;
  };
  return raw.map((e) => resolve(e));
}

// ------------------------------------------------------------------ pkt-lines

function pktLine(payload: string | Buffer): Buffer {
  const body = typeof payload === 'string' ? Buffer.from(payload, 'utf8') : payload;
  const len = body.length + 4;
  return Buffer.concat([Buffer.from(len.toString(16).padStart(4, '0'), 'ascii'), body]);
}
const PKT_FLUSH = Buffer.from('0000', 'ascii');
const PKT_DELIM = Buffer.from('0001', 'ascii');
function pktLines(items: (string | Buffer | 'flush' | 'delim')[]): Buffer {
  const parts: Buffer[] = [];
  for (const it of items) {
    if (it === 'flush') parts.push(PKT_FLUSH);
    else if (it === 'delim') parts.push(PKT_DELIM);
    else parts.push(pktLine(it));
  }
  return Buffer.concat(parts);
}
type Pkt = { kind: 'data' | 'flush' | 'delim'; payload: Buffer };
function decodePktLines(buf: Buffer): Pkt[] {
  const out: Pkt[] = [];
  let off = 0;
  while (off < buf.length) {
    if (off + 4 > buf.length) throw new Error('truncated pkt-line');
    const len = parseInt(buf.subarray(off, off + 4).toString('ascii'), 16);
    if (Number.isNaN(len)) throw new Error('bad pkt-line length');
    if (len === 0) {
      out.push({ kind: 'flush', payload: Buffer.alloc(0) });
      off += 4;
    } else if (len === 1) {
      out.push({ kind: 'delim', payload: Buffer.alloc(0) });
      off += 4;
    } else {
      if (off + len > buf.length) throw new Error('truncated pkt-line payload');
      out.push({ kind: 'data', payload: buf.subarray(off + 4, off + len) });
      off += len;
    }
  }
  return out;
}
function pktDataLines(buf: Buffer): string[] {
  return decodePktLines(buf).filter((p) => p.kind === 'data').map((p) => p.payload.toString('utf8'));
}

// ------------------------------------------------------------------ store

interface Issue {
  id: number; number: number; title: string; body: string; state: 'open' | 'closed';
  labels: string[]; seq: number; createdAt: string; updatedAt: string;
  comments: { id: number; body: string; createdAt: string }[];
}
interface Pull {
  id: number; number: number; title: string; body: string; state: 'open' | 'closed';
  merged: boolean; mergedAt: string | null; mergeCommitSha: string | null;
  headBranch: string; baseBranch: string; headSha: string; baseSha: string; seq: number; createdAt: string;
}
interface Repo {
  name: string; id: number; description: string; isPrivate: boolean;
  defaultBranch: string; seq: number; createdAt: string;
  refs: Map<string, string>;
  labels: Map<string, { name: string; color: string }>;
  issues: Map<number, Issue>;
  pulls: Map<number, Pull>;
  nextNumber: number;
  packCache: Map<string, Buffer>;
}
const repos = new Map<string, Repo>();
let repoIdCounter = 0;
let entityIdCounter = 0;
function nextEntityId(): number {
  entityIdCounter += 1;
  return entityIdCounter;
}
function createRepoRecord(name: string, isPrivate: boolean, description: string): Repo {
  repoIdCounter += 1;
  const sec = tick();
  const repo: Repo = {
    name, id: repoIdCounter, description, isPrivate, defaultBranch: 'main', seq: repoIdCounter,
    createdAt: isoOf(sec), refs: new Map(), labels: new Map(), issues: new Map(), pulls: new Map(),
    nextNumber: 1, packCache: new Map(),
  };
  repos.set(name, repo);
  return repo;
}
function getRepo(owner: string, name: string): Repo | undefined {
  if (owner !== VIEWER_LOGIN) return undefined;
  return repos.get(name);
}
function repoJson(r: Repo): Record<string, unknown> {
  return {
    id: r.id, node_id: gid('repo', `${VIEWER_LOGIN}/${r.name}`), name: r.name, full_name: `${VIEWER_LOGIN}/${r.name}`,
    owner: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User', avatar_url: '' },
    private: r.isPrivate, html_url: '', description: r.description, fork: false,
    url: '', created_at: r.createdAt, updated_at: r.createdAt, pushed_at: r.createdAt,
    default_branch: r.defaultBranch, has_issues: true, has_projects: true, has_wiki: true,
    has_pages: false, has_downloads: true, archived: false, disabled: false, visibility: r.isPrivate ? 'private' : 'public',
    license: null, topics: [], open_issues_count: [...r.issues.values()].filter((i) => i.state === 'open').length,
  };
}
function issueJson(r: Repo, i: Issue): Record<string, unknown> {
  return {
    id: i.id, node_id: gid('issue', `${VIEWER_LOGIN}/${r.name}:${i.number}`), number: i.number,
    title: i.title, body: i.body, state: i.state,
    user: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' },
    labels: i.labels.map((n) => ({ name: n, color: r.labels.get(n)?.color ?? 'ededed' })),
    assignee: null, assignees: [], milestone: null, locked: false,
    comments: i.comments.length, created_at: i.createdAt, updated_at: i.updatedAt, closed_at: i.state === 'closed' ? i.updatedAt : null,
    author_association: 'OWNER', html_url: '', url: '',
    repository_url: '', labels_url: '', comments_url: '',
  };
}
function pullJson(r: Repo, p: Pull): Record<string, unknown> {
  return {
    id: p.id, node_id: gid('pr', `${VIEWER_LOGIN}/${r.name}:${p.number}`), number: p.number,
    title: p.title, body: p.body, state: p.state, merged: p.merged,
    merged_at: p.mergedAt, merge_commit_sha: p.mergeCommitSha, mergeable: prMergeable(r, p) === 'MERGEABLE',
    mergeable_state: prMergeable(r, p).toLowerCase(),
    head: { label: `${VIEWER_LOGIN}:${p.headBranch}`, ref: p.headBranch, sha: p.headSha, user: { login: VIEWER_LOGIN } },
    base: { label: `${VIEWER_LOGIN}:${p.baseBranch}`, ref: p.baseBranch, sha: p.baseSha, user: { login: VIEWER_LOGIN } },
    user: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' },
    created_at: p.createdAt, updated_at: p.createdAt, closed_at: p.state === 'closed' ? p.createdAt : null,
    draft: false, locked: false, html_url: '', url: '', commits: 1, additions: 0, deletions: 0, changed_files: 0,
  };
}
function gid(kind: string, key: string): string {
  return Buffer.from(`${kind}:${key}`, 'utf8').toString('base64url');
}
function decodeGid(id: string): { kind: string; key: string } | null {
  try {
    const text = Buffer.from(id, 'base64url').toString('utf8');
    const idx = text.indexOf(':');
    if (idx < 0) return null;
    return { kind: text.slice(0, idx), key: text.slice(idx + 1) };
  } catch {
    return null;
  }
}

// Flatten a commit's tree to path -> {oid, mode}.
function flattenTree(treeOid: string, prefix = ''): Map<string, { oid: string; mode: string }> {
  const out = new Map<string, { oid: string; mode: string }>();
  const obj = objects.get(treeOid);
  if (!obj || obj.type !== 'tree') return out;
  for (const e of parseTree(obj.data)) {
    const path = prefix === '' ? e.name : `${prefix}/${e.name}`;
    if (e.mode === '40000') {
      for (const [k, v] of flattenTree(e.oid, path)) out.set(k, v);
    } else {
      out.set(path, { oid: e.oid, mode: e.mode });
    }
  }
  return out;
}
/** Every object id in a tree, recursively: the tree itself, subtrees, blobs. */
function treeObjectClosure(treeOid: string, out: Set<string>): void {
  const obj = objects.get(treeOid);
  if (!obj || obj.type !== 'tree') return;
  if (out.has(treeOid)) return;
  out.add(treeOid);
  for (const e of parseTree(obj.data)) {
    if (e.mode === '40000') treeObjectClosure(e.oid, out);
    else out.add(e.oid);
  }
}
// Build a tree object from a flat path map; returns the root tree oid.
function buildTreeFromFlat(flat: Map<string, { oid: string; mode: string }>): string {
  interface Node { files: Map<string, { oid: string; mode: string }>; dirs: Map<string, Node> }
  const make = (): Node => ({ files: new Map(), dirs: new Map() });
  const root = make();
  for (const [path, v] of flat) {
    const segs = path.split('/');
    let node = root;
    for (let i = 0; i < segs.length - 1; i++) {
      const s = segs[i]!;
      if (!node.dirs.has(s)) node.dirs.set(s, make());
      node = node.dirs.get(s)!;
    }
    node.files.set(segs[segs.length - 1]!, v);
  }
  const build = (node: Node): string => {
    const entries: TreeEntry[] = [];
    for (const [name, v] of node.files) entries.push({ mode: v.mode, name, oid: v.oid });
    for (const [name, child] of node.dirs) entries.push({ mode: '40000', name, oid: build(child) });
    return putObject('tree', buildTree(entries));
  };
  return build(root);
}
function commitForRef(repo: Repo, ref: string): string | undefined {
  return repo.refs.get(ref);
}
function makeCommit(repo: Repo, parent: string | undefined, flat: Map<string, { oid: string; mode: string }>, message: string): string {
  const treeOid = buildTreeFromFlat(flat);
  const ts = tick();
  const ident = `${VIEWER_NAME} <${VIEWER_EMAIL}>`;
  const data = buildCommit(treeOid, parent ? [parent] : [], message, ident, ident, ts);
  const oid = putObject('commit', data);
  repo.packCache.clear();
  return oid;
}
/** Commit one file change on a branch; returns the new commit oid. */
function commitFile(repo: Repo, branch: string, path: string, content: Buffer | null, message: string): string {
  const tip = repo.refs.get(`refs/heads/${branch}`);
  const flat = tip ? flattenTree(parseCommit(objects.get(tip)!.data).tree) : new Map();
  if (content === null) flat.delete(path);
  else flat.set(path, { oid: putObject('blob', content), mode: '100644' });
  const oid = makeCommit(repo, tip, flat, message);
  repo.refs.set(`refs/heads/${branch}`, oid);
  return oid;
}
function blobAt(repo: Repo, refName: string, path: string): { oid: string; data: Buffer } | null {
  const tip = repo.refs.get(refName);
  if (!tip) return null;
  const flat = flattenTree(parseCommit(objects.get(tip)!.data).tree);
  const e = flat.get(path);
  if (!e) return null;
  const obj = objects.get(e.oid);
  if (!obj || obj.type !== 'blob') return null;
  return { oid: e.oid, data: obj.data };
}
function mergeBase(a: string, b: string): string | null {
  const ancA = new Set<string>();
  const q = [a];
  while (q.length) {
    const cur = q.pop()!;
    if (ancA.has(cur)) continue;
    ancA.add(cur);
    const o = objects.get(cur);
    if (o?.type === 'commit') for (const p of parseCommit(o.data).parents) q.push(p);
  }
  const q2 = [b];
  const seen = new Set<string>();
  while (q2.length) {
    const cur = q2.pop()!;
    if (seen.has(cur)) continue;
    seen.add(cur);
    if (ancA.has(cur)) return cur;
    const o = objects.get(cur);
    if (o?.type === 'commit') for (const p of parseCommit(o.data).parents) q2.push(p);
  }
  return null;
}
/** Three-way conflict test: a path changed on both sides of the merge-base differently. */
function prConflicts(r: Repo, p: Pull): boolean {
  const curTip = r.refs.get(`refs/heads/${p.baseBranch}`) ?? p.baseSha;
  const base = mergeBase(p.headSha, curTip);
  if (!base) return false;
  const baseFlat = flattenTree(parseCommit(objects.get(base)!.data).tree);
  const headFlat = flattenTree(parseCommit(objects.get(p.headSha)!.data).tree);
  const curFlat = flattenTree(parseCommit(objects.get(curTip)!.data).tree);
  const paths = new Set([...baseFlat.keys(), ...headFlat.keys(), ...curFlat.keys()]);
  for (const path of paths) {
    const b = baseFlat.get(path)?.oid ?? null;
    const h = headFlat.get(path)?.oid ?? null;
    const c = curFlat.get(path)?.oid ?? null;
    if (h !== b && c !== b && h !== c) return true;
  }
  return false;
}
function prMergeable(r: Repo, p: Pull): 'MERGEABLE' | 'CONFLICTING' {
  if (p.state !== 'open') return 'CONFLICTING';
  return prConflicts(r, p) ? 'CONFLICTING' : 'MERGEABLE';
}
function prFiles(r: Repo, p: Pull): { filename: string; status: string; additions: number; deletions: number; changes: number }[] {
  const baseFlat = flattenTree(parseCommit(objects.get(p.baseSha)!.data).tree);
  const headFlat = flattenTree(parseCommit(objects.get(p.headSha)!.data).tree);
  const out: { filename: string; status: string; additions: number; deletions: number; changes: number }[] = [];
  const paths = new Set([...baseFlat.keys(), ...headFlat.keys()]);
  for (const path of [...paths].sort()) {
    const b = baseFlat.get(path);
    const h = headFlat.get(path);
    if (b?.oid === h?.oid) continue;
    const bText = b ? (objects.get(b.oid)?.data.toString('utf8') ?? '') : '';
    const hText = h ? (objects.get(h.oid)?.data.toString('utf8') ?? '') : '';
    const bLines = bText === '' ? [] : bText.split('\n');
    const hLines = hText === '' ? [] : hText.split('\n');
    const bSet = new Map<string, number>();
    for (const l of bLines) bSet.set(l, (bSet.get(l) ?? 0) + 1);
    let additions = 0;
    for (const l of hLines) {
      const c = bSet.get(l) ?? 0;
      if (c > 0) bSet.set(l, c - 1);
      else additions += 1;
    }
    const hSet = new Map<string, number>();
    for (const l of hLines) hSet.set(l, (hSet.get(l) ?? 0) + 1);
    let deletions = 0;
    for (const l of bLines) {
      const c = hSet.get(l) ?? 0;
      if (c > 0) hSet.set(l, c - 1);
      else deletions += 1;
    }
    if (!b) additions = Math.max(additions, 1);
    const status = !b ? 'added' : !h ? 'removed' : 'modified';
    out.push({ filename: path, status, additions, deletions, changes: additions + deletions });
  }
  return out;
}

// ------------------------------------------------------------------ http utils

const JSON_TYPE = 'application/json; charset=utf-8';
function sendJson(res: ServerResponse, status: number, body: unknown, extra: Record<string, string> = {}): void {
  const text = `${JSON.stringify(body)}\n`;
  res.writeHead(status, { 'content-type': JSON_TYPE, 'content-length': Buffer.byteLength(text), 'x-github-request-id': nextReqId(), ...extra });
  res.end(text);
}
function sendErr(res: ServerResponse, status: number, message: string, extra: Record<string, string> = {}): void {
  sendJson(res, status, { message }, extra);
}
function readBody(req: IncomingMessage): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    let size = 0;
    req.on('data', (c: Buffer) => {
      size += c.length;
      if (size > 64 * 1024 * 1024) {
        req.destroy();
        reject(new Error('body too large'));
        return;
      }
      chunks.push(c);
    });
    req.on('end', () => resolve(Buffer.concat(chunks)));
    req.on('error', reject);
  });
}
function parseJsonBody(buf: Buffer): { ok: true; value: unknown } | { ok: false } {
  if (buf.length === 0) return { ok: true, value: {} };
  try {
    return { ok: true, value: JSON.parse(buf.toString('utf8')) };
  } catch {
    return { ok: false };
  }
}
function isAuthed(req: IncomingMessage): boolean {
  if (TOKEN === '') return true;
  const h = req.headers.authorization ?? '';
  return h === `Bearer ${TOKEN}`;
}
function decodePath(p: string): string {
  try {
    return decodeURIComponent(p);
  } catch {
    return p;
  }
}
function paginate<T>(items: T[], query: URLSearchParams, req: IncomingMessage, basePath: string): { page: T[]; link: string | null } {
  const perPage = Math.max(1, Math.min(100, Number(query.get('per_page') ?? '30') || 30));
  const pageNum = Math.max(1, Number(query.get('page') ?? '1') || 1);
  const totalPages = Math.max(1, Math.ceil(items.length / perPage));
  const start = (pageNum - 1) * perPage;
  const page = items.slice(start, start + perPage);
  const host = req.headers.host ?? 'localhost';
  const proto = 'http';
  let link: string | null = null;
  if (pageNum < totalPages) {
    const q = new URLSearchParams(query);
    q.set('page', String(pageNum + 1));
    q.set('per_page', String(perPage));
    const next = `${proto}://${host}${basePath}?${q.toString()}`;
    const qLast = new URLSearchParams(query);
    qLast.set('page', String(totalPages));
    qLast.set('per_page', String(perPage));
    link = `<${next}>; rel="next", <${proto}://${host}${basePath}?${qLast.toString()}>; rel="last"`;
  }
  return { page, link };
}
function linkHeader(link: string | null): Record<string, string> {
  return link ? { link } : {};
}

// ------------------------------------------------------------------ REST v3

type Handler = (req: IncomingMessage, res: ServerResponse, params: string[], query: URLSearchParams, body: Buffer) => void | Promise<void>;

async function handleRest(req: IncomingMessage, res: ServerResponse, path: string, query: URLSearchParams, body: Buffer): Promise<boolean> {
  const method = req.method ?? 'GET';
  const segs = path.split('/').filter((s) => s !== '').map(decodePath);
  // segs[0..1] = api, v3
  if (segs[0] !== 'api' || segs[1] !== 'v3') return false;
  const rest = segs.slice(2);
  if (!isAuthed(req)) {
    sendErr(res, 401, 'Bad credentials');
    return true;
  }
  const jsonBody = async (): Promise<{ ok: true; value: Record<string, unknown> } | { ok: false }> => {
    if (method === 'GET' || method === 'DELETE' && body.length === 0) return { ok: true, value: {} };
    const parsed = parseJsonBody(body);
    if (!parsed.ok) return { ok: false };
    const v = parsed.value;
    if (typeof v !== 'object' || v === null || Array.isArray(v)) return { ok: true, value: {} };
    return { ok: true, value: v as Record<string, unknown> };
  };
  const badJson = (): boolean => {
    sendErr(res, 400, 'Problems parsing JSON');
    return true;
  };

  // GET /api/v3/ and /api/v3/meta
  if (rest.length === 0) {
    if (method !== 'GET') { sendErr(res, 404, 'Not Found'); return true; }
    sendJson(res, 200, { current_user_url: '/api/v3/user', rate_limit_url: '/api/v3/rate_limit' }, { 'x-oauth-scopes': 'repo, read:org' });
    return true;
  }
  if (rest.length === 1 && rest[0] === 'meta') {
    sendJson(res, 200, { installed_version: '1.0.0', verifiable_password_authentication: false, github_services_sha: '', hook_ip_ranges: [], import_ip_ranges: [], ssh_key_fingerprints: {}, pages: {} });
    return true;
  }
  if (rest.length === 1 && rest[0] === 'user' && method === 'GET') {
    sendJson(res, 200, { login: VIEWER_LOGIN, id: VIEWER_ID, node_id: gid('user', VIEWER_LOGIN), type: 'User', name: VIEWER_NAME, email: VIEWER_EMAIL, avatar_url: '' });
    return true;
  }
  // GET/POST /api/v3/user/repos
  if (rest.length === 2 && rest[0] === 'user' && rest[1] === 'repos') {
    if (method === 'GET') {
      const all = [...repos.values()].sort((a, b) => a.seq - b.seq).map(repoJson);
      const { page, link } = paginate(all, query, req, '/api/v3/user/repos');
      sendJson(res, 200, page, linkHeader(link));
      return true;
    }
    if (method === 'POST') {
      const parsed = await jsonBody();
      if (!parsed.ok) return badJson();
      const name = String(parsed.value.name ?? '');
      if (name === '') { sendErr(res, 422, 'Repository name is required'); return true; }
      if (repos.has(name)) { sendErr(res, 422, 'Repository already exists'); return true; }
      const repo = createRepoRecord(name, parsed.value.private === true, String(parsed.value.description ?? ''));
      sendJson(res, 201, repoJson(repo));
      return true;
    }
    sendErr(res, 404, 'Not Found');
    return true;
  }
  // /api/v3/repos/{owner}/{repo}[...]
  if (rest.length >= 3 && rest[0] === 'repos') {
    const owner = rest[1]!;
    const name = rest[2]!;
    const repo = getRepo(owner, name);
    const tail = rest.slice(3);
    if (!repo) { sendErr(res, 404, 'Not Found'); return true; }
    if (tail.length === 0) {
      if (method === 'GET') { sendJson(res, 200, repoJson(repo)); return true; }
      if (method === 'PATCH') {
        const parsed = await jsonBody();
        if (!parsed.ok) return badJson();
        if (typeof parsed.value.description === 'string') repo.description = parsed.value.description;
        if (typeof parsed.value.private === 'boolean') repo.isPrivate = parsed.value.private;
        sendJson(res, 200, repoJson(repo));
        return true;
      }
      if (method === 'DELETE') {
        repos.delete(name);
        res.writeHead(204, { 'x-github-request-id': nextReqId() });
        res.end();
        return true;
      }
      sendErr(res, 404, 'Not Found');
      return true;
    }
    // branches
    if (tail.length === 1 && tail[0] === 'branches' && method === 'GET') {
      const list = [...repo.refs.entries()].filter(([r]) => r.startsWith('refs/heads/')).sort(([a], [b]) => (a < b ? -1 : 1))
        .map(([r, sha]) => ({ name: r.slice('refs/heads/'.length), commit: { sha, url: '' }, protected: false }));
      sendJson(res, 200, list);
      return true;
    }
    if (tail.length === 2 && tail[0] === 'branches' && method === 'GET') {
      const sha = repo.refs.get(`refs/heads/${tail[1]}`);
      if (!sha) { sendErr(res, 404, 'Not Found'); return true; }
      sendJson(res, 200, { name: tail[1], commit: { sha, url: '' }, protected: false });
      return true;
    }
    // commits
    if (tail.length === 1 && tail[0] === 'commits' && method === 'GET') {
      const refName = query.get('sha');
      const tip = refName ? (repo.refs.get(`refs/heads/${refName}`) ?? (repo.refs.get(refName) ?? (/^[0-9a-f]{40}$/.test(refName) ? refName : undefined))) : repo.refs.get(`refs/heads/${repo.defaultBranch}`);
      const list: Record<string, unknown>[] = [];
      if (tip) {
        const seen = new Set<string>();
        const stack = [tip];
        while (stack.length) {
          const oid = stack.pop()!;
          if (seen.has(oid)) continue;
          seen.add(oid);
          const obj = objects.get(oid);
          if (!obj || obj.type !== 'commit') continue;
          const c = parseCommit(obj.data);
          const authorLine = c.author;
          list.push({
            sha: oid, commit: { message: c.message, author: { name: authorLine.split(' <')[0] ?? '', email: VIEWER_EMAIL, date: isoOf(EPOCH_SEC) }, committer: { name: VIEWER_NAME, email: VIEWER_EMAIL, date: isoOf(EPOCH_SEC) } },
            author: { login: VIEWER_LOGIN, id: VIEWER_ID }, committer: { login: VIEWER_LOGIN, id: VIEWER_ID },
            parents: c.parents.map((p) => ({ sha: p })),
          });
          for (const p of c.parents) stack.push(p);
        }
      }
      sendJson(res, 200, list);
      return true;
    }
    // git/refs
    if (tail[0] === 'git' && tail[1] === 'refs') {
      if (method === 'POST' && tail.length === 2) {
        const parsed = await jsonBody();
        if (!parsed.ok) return badJson();
        const ref = String(parsed.value.ref ?? '');
        const sha = String(parsed.value.sha ?? '');
        if (!ref.startsWith('refs/') || !/^[0-9a-f]{40}$/.test(sha)) { sendErr(res, 422, 'Invalid ref'); return true; }
        repo.refs.set(ref, sha);
        repo.packCache.clear();
        sendJson(res, 201, { ref, object: { sha, type: 'commit' } });
        return true;
      }
      if (method === 'GET' && tail.length >= 3) {
        const ref = tail.slice(2).join('/');
        const full = ref.startsWith('refs/') ? ref : `refs/${ref}`;
        const sha = repo.refs.get(full);
        if (!sha) { sendErr(res, 404, 'Not Found'); return true; }
        sendJson(res, 200, { ref: full, object: { sha, type: 'commit' } });
        return true;
      }
      sendErr(res, 404, 'Not Found');
      return true;
    }
    // contents
    if (tail[0] === 'contents') {
      const filePath = tail.slice(1).join('/');
      const refParam = query.get('ref');
      const refName = refParam
        ? (repo.refs.has(`refs/heads/${refParam}`) ? `refs/heads/${refParam}` : (repo.refs.has(refParam) ? refParam : (/^[0-9a-f]{40}$/.test(refParam) ? refParam : `refs/heads/${repo.defaultBranch}`)))
        : `refs/heads/${repo.defaultBranch}`;
      if (method === 'GET') {
        const tip = /^[0-9a-f]{40}$/.test(refName) ? refName : repo.refs.get(refName);
        if (!tip) { sendErr(res, 404, 'Not Found'); return true; }
        const treeOid = parseCommit(objects.get(tip)!.data).tree;
        const flat = flattenTree(treeOid);
        if (filePath === '') {
          const entries: Record<string, unknown>[] = [];
          const seen = new Set<string>();
          for (const p of [...flat.keys()].sort()) {
            const first = p.split('/')[0]!;
            if (seen.has(first)) continue;
            seen.add(first);
            const isDir = ![...flat.keys()].some((k) => k === first) && [...flat.keys()].some((k) => k.startsWith(`${first}/`));
            const oid = isDir ? '' : flat.get(first)!.oid;
            entries.push({ type: isDir ? 'dir' : 'file', name: first, path: first, sha: oid || '0'.repeat(40), size: isDir ? 0 : (objects.get(oid)?.data.length ?? 0) });
          }
          sendJson(res, 200, entries);
          return true;
        }
        const entry = flat.get(filePath);
        if (entry) {
          const data = objects.get(entry.oid)!.data;
          sendJson(res, 200, { type: 'file', encoding: 'base64', size: data.length, name: filePath.split('/').pop(), path: filePath, content: data.toString('base64'), sha: entry.oid });
          return true;
        }
        // directory?
        const children = [...flat.keys()].filter((p) => p.startsWith(`${filePath}/`));
        if (children.length) {
          const entries: Record<string, unknown>[] = [];
          const seen = new Set<string>();
          for (const p of children.sort()) {
            const restPath = p.slice(filePath.length + 1);
            const first = restPath.split('/')[0]!;
            if (seen.has(first)) continue;
            seen.add(first);
            const isDir = restPath.includes('/');
            const oid = isDir ? '' : flat.get(p)!.oid;
            entries.push({ type: isDir ? 'dir' : 'file', name: first, path: `${filePath}/${first}`, sha: oid || '0'.repeat(40), size: isDir ? 0 : (objects.get(oid)?.data.length ?? 0) });
          }
          sendJson(res, 200, entries);
          return true;
        }
        sendErr(res, 404, 'Not Found');
        return true;
      }
      if (method === 'PUT' || method === 'DELETE') {
        const parsed = await jsonBody();
        if (!parsed.ok) return badJson();
        const branch = typeof parsed.value.branch === 'string' ? parsed.value.branch : repo.defaultBranch;
        const refKey = `refs/heads/${branch}`;
        const tip = repo.refs.get(refKey);
        const flat = tip ? flattenTree(parseCommit(objects.get(tip)!.data).tree) : new Map<string, { oid: string; mode: string }>();
        const existing = flat.get(filePath);
        if (method === 'PUT') {
          const contentB64 = String(parsed.value.content ?? '');
          if (existing && typeof parsed.value.sha === 'string' && parsed.value.sha !== existing.oid) { sendErr(res, 409, 'Conflict'); return true; }
          if (existing && parsed.value.sha === undefined) { sendErr(res, 409, 'Conflict'); return true; }
          const content = Buffer.from(contentB64.replace(/[\r\n\t ]+/g, ''), 'base64');
          const message = String(parsed.value.message ?? 'Update');
          const commitSha = commitFile(repo, branch, filePath, content, message);
          const blobOid = putObject('blob', content);
          sendJson(res, existing ? 200 : 201, { content: { name: filePath.split('/').pop(), path: filePath, sha: blobOid, size: content.length, encoding: 'base64' }, commit: { sha: commitSha, message } });
          return true;
        }
        // DELETE contents
        if (!existing) { sendErr(res, 404, 'Not Found'); return true; }
        if (typeof parsed.value.sha === 'string' && parsed.value.sha !== existing.oid) { sendErr(res, 409, 'Conflict'); return true; }
        const message = String(parsed.value.message ?? 'Delete');
        const commitSha = commitFile(repo, branch, filePath, null, message);
        sendJson(res, 200, { commit: { sha: commitSha, message } });
        return true;
      }
      sendErr(res, 404, 'Not Found');
      return true;
    }
    // labels
    if (tail.length === 1 && tail[0] === 'labels') {
      if (method === 'GET') {
        sendJson(res, 200, [...repo.labels.values()].map((l) => ({ id: nextEntityId(), name: l.name, color: l.color, default: false, description: '' })));
        return true;
      }
      if (method === 'POST') {
        const parsed = await jsonBody();
        if (!parsed.ok) return badJson();
        const name = String(parsed.value.name ?? '');
        if (name === '') { sendErr(res, 422, 'Label name is required'); return true; }
        const color = String(parsed.value.color ?? 'ededed');
        repo.labels.set(name, { name, color });
        sendJson(res, 201, { id: nextEntityId(), name, color, default: false, description: '' });
        return true;
      }
      sendErr(res, 404, 'Not Found');
      return true;
    }
    // issues
    if (tail[0] === 'issues') {
      if (tail.length === 1) {
        if (method === 'GET') {
          const state = query.get('state') ?? 'open';
          let list = [...repo.issues.values()];
          if (state !== 'all') list = list.filter((i) => i.state === state);
          const sort = query.get('sort') ?? 'created';
          const dir = query.get('direction') ?? 'desc';
          if (sort === 'created') list.sort((a, b) => (dir === 'asc' ? a.seq - b.seq : b.seq - a.seq));
          const { page, link } = paginate(list.map((i) => issueJson(repo, i)), query, req, `/api/v3/repos/${owner}/${name}/issues`);
          sendJson(res, 200, page, linkHeader(link));
          return true;
        }
        if (method === 'POST') {
          const parsed = await jsonBody();
          if (!parsed.ok) return badJson();
          const labelsRaw = parsed.value.labels;
          const labelNames: string[] = [];
          if (Array.isArray(labelsRaw)) {
            for (const l of labelsRaw) {
              const ln = typeof l === 'string' ? l : (l as Record<string, unknown>)?.name;
              if (typeof ln !== 'string') continue;
              if (!repo.labels.has(ln)) { sendErr(res, 422, `Label does not exist: ${ln}`); return true; }
              labelNames.push(ln);
            }
          }
          const number = repo.nextNumber++;
          const sec = tick();
          const issue: Issue = {
            id: nextEntityId(), number, title: String(parsed.value.title ?? ''), body: String(parsed.value.body ?? ''),
            state: 'open', labels: labelNames, seq: number, createdAt: isoOf(sec), updatedAt: isoOf(sec), comments: [],
          };
          repo.issues.set(number, issue);
          sendJson(res, 201, issueJson(repo, issue));
          return true;
        }
        sendErr(res, 404, 'Not Found');
        return true;
      }
      const num = Number(tail[1]);
      if (!Number.isInteger(num)) { sendErr(res, 404, 'Not Found'); return true; }
      const issue = repo.issues.get(num);
      if (tail.length === 2) {
        if (!issue) { sendErr(res, 404, 'Not Found'); return true; }
        if (method === 'GET') { sendJson(res, 200, issueJson(repo, issue)); return true; }
        if (method === 'PATCH') {
          const parsed = await jsonBody();
          if (!parsed.ok) return badJson();
          if (parsed.value.state === 'open' || parsed.value.state === 'closed') issue.state = parsed.value.state;
          if (typeof parsed.value.title === 'string') issue.title = parsed.value.title;
          if (typeof parsed.value.body === 'string') issue.body = parsed.value.body;
          issue.updatedAt = nowIso();
          sendJson(res, 200, issueJson(repo, issue));
          return true;
        }
        sendErr(res, 404, 'Not Found');
        return true;
      }
      if (tail.length === 3 && tail[2] === 'comments') {
        if (!issue) { sendErr(res, 404, 'Not Found'); return true; }
        if (method === 'GET') {
          sendJson(res, 200, issue.comments.map((c) => ({ id: c.id, node_id: gid('comment', `${VIEWER_LOGIN}/${name}:${num}:${c.id}`), body: c.body, user: { login: VIEWER_LOGIN, id: VIEWER_ID }, created_at: c.createdAt, updated_at: c.createdAt })));
          return true;
        }
        if (method === 'POST') {
          const parsed = await jsonBody();
          if (!parsed.ok) return badJson();
          const comment = { id: nextEntityId(), body: String(parsed.value.body ?? ''), createdAt: nowIso() };
          issue.comments.push(comment);
          sendJson(res, 201, { id: comment.id, node_id: gid('comment', `${VIEWER_LOGIN}/${name}:${num}:${comment.id}`), body: comment.body, user: { login: VIEWER_LOGIN, id: VIEWER_ID }, created_at: comment.createdAt, updated_at: comment.createdAt });
          return true;
        }
        sendErr(res, 404, 'Not Found');
        return true;
      }
      sendErr(res, 404, 'Not Found');
      return true;
    }
    // pulls
    if (tail[0] === 'pulls') {
      if (tail.length === 1) {
        if (method === 'GET') {
          const state = query.get('state') ?? 'open';
          let list = [...repo.pulls.values()];
          if (state !== 'all') list = list.filter((p) => p.state === state);
          list.sort((a, b) => b.seq - a.seq);
          const { page, link } = paginate(list.map((p) => pullJson(repo, p)), query, req, `/api/v3/repos/${owner}/${name}/pulls`);
          sendJson(res, 200, page, linkHeader(link));
          return true;
        }
        if (method === 'POST') {
          const parsed = await jsonBody();
          if (!parsed.ok) return badJson();
          const headBranch = String(parsed.value.head ?? '');
          const baseBranch = String(parsed.value.base ?? repo.defaultBranch);
          const headSha = repo.refs.get(`refs/heads/${headBranch}`);
          const baseSha = repo.refs.get(`refs/heads/${baseBranch}`);
          if (!headSha || !baseSha) { sendErr(res, 422, 'Head or base branch not found'); return true; }
          const existing = [...repo.pulls.values()].find((p) => p.state === 'open' && p.headBranch === headBranch && p.baseBranch === baseBranch);
          if (existing) { sendErr(res, 422, 'A pull request already exists for this branch'); return true; }
          const number = repo.nextNumber++;
          const sec = tick();
          const pr: Pull = {
            id: nextEntityId(), number, title: String(parsed.value.title ?? ''), body: String(parsed.value.body ?? ''),
            state: 'open', merged: false, mergedAt: null, mergeCommitSha: null,
            headBranch, baseBranch, headSha, baseSha, seq: number, createdAt: isoOf(sec),
          };
          repo.pulls.set(number, pr);
          sendJson(res, 201, pullJson(repo, pr));
          return true;
        }
        sendErr(res, 404, 'Not Found');
        return true;
      }
      const num = Number(tail[1]);
      if (!Number.isInteger(num)) { sendErr(res, 404, 'Not Found'); return true; }
      const pr = repo.pulls.get(num);
      if (tail.length === 2) {
        if (!pr) { sendErr(res, 404, 'Not Found'); return true; }
        if (method === 'GET') { sendJson(res, 200, pullJson(repo, pr)); return true; }
        sendErr(res, 404, 'Not Found');
        return true;
      }
      if (tail.length === 3 && tail[2] === 'files' && method === 'GET') {
        if (!pr) { sendErr(res, 404, 'Not Found'); return true; }
        sendJson(res, 200, prFiles(repo, pr));
        return true;
      }
      if (tail.length === 3 && tail[2] === 'merge' && method === 'PUT') {
        if (!pr) { sendErr(res, 404, 'Not Found'); return true; }
        if (pr.state !== 'open') { sendErr(res, 405, 'Pull request is not open'); return true; }
        if (prConflicts(repo, pr)) { sendErr(res, 405, 'Merge conflict'); return true; }
        const headFlat = flattenTree(parseCommit(objects.get(pr.headSha)!.data).tree);
        const baseTip = repo.refs.get(`refs/heads/${pr.baseBranch}`);
        const mergeSha = makeCommit(repo, baseTip, headFlat, `Merge pull request #${pr.number} from ${pr.headBranch}\n\n${pr.title}`);
        repo.refs.set(`refs/heads/${pr.baseBranch}`, mergeSha);
        pr.state = 'closed';
        pr.merged = true;
        pr.mergedAt = nowIso();
        pr.mergeCommitSha = mergeSha;
        pr.baseSha = mergeSha;
        sendJson(res, 200, { sha: mergeSha, merged: true, message: 'Pull Request successfully merged' });
        return true;
      }
      sendErr(res, 404, 'Not Found');
      return true;
    }
    sendErr(res, 404, 'Not Found');
    return true;
  }
  sendErr(res, 404, 'Not Found');
  return true;
}

// ------------------------------------------------------------------ git smart HTTP

function sortedRefs(repo: Repo): [string, string][] {
  return [...repo.refs.entries()].sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0));
}
function refCaps(repo: Repo, service: 'upload' | 'receive'): string {
  const symref = `symref=HEAD:refs/heads/${repo.defaultBranch}`;
  if (service === 'upload') return `side-band-64k ofs-delta thin-pack shallow multi_ack ${symref} agent=github-core-clone/1.0`;
  return `report-status side-band-64k delete-refs ofs-delta ${symref} agent=github-core-clone/1.0`;
}
function v0Advertisement(repo: Repo, service: 'upload' | 'receive'): Buffer {
  const caps = refCaps(repo, service);
  const refs = sortedRefs(repo);
  const lines: (string | 'flush')[] = [];
  if (refs.length === 0) {
    lines.push(`${ZERO_OID} capabilities^{}\0${caps}\n`);
  } else {
    const headSha = repo.refs.get(`refs/heads/${repo.defaultBranch}`) ?? refs[0]![1];
    lines.push(`${headSha} HEAD\0${caps}\n`);
    for (const [name, oid] of refs) {
      lines.push(`${oid} ${name}\n`);
      const obj = objects.get(oid);
      if (name.startsWith('refs/tags/') && obj?.type === 'tag') {
        lines.push(`${peelToCommit(oid)} ${name}^{}\n`);
      }
    }
  }
  lines.push('flush');
  const serviceName = service === 'upload' ? 'git-upload-pack' : 'git-receive-pack';
  return Buffer.concat([pktLine(`# service=${serviceName}\n`), PKT_FLUSH, pktLines(lines)]);
}
function v2Advertisement(): Buffer {
  return pktLines([
    'version 2\n',
    'ls-refs=unborn\n',
    'fetch=shallow filter\n',
    'object-format=sha1\n',
    'agent=github-core-clone/1.0\n',
    'flush',
  ]);
}

/** Objects to send for a fetch, honouring wants/haves/depth/shallow boundaries. */
function fetchObjects(repo: Repo, wants: string[], haves: string[], depth: number | undefined, shallow: string[]): { objs: { type: ObjType; data: Buffer }[]; newShallow: string[]; unshallow: string[] } {
  // Depth-limited closure from wants.
  const wanted = new Set<string>();
  const wantCommits = new Set<string>();
  const queue: { oid: string; d: number }[] = wants.map((w) => ({ oid: w, d: 0 }));
  const seenQ = new Set<string>();
  while (queue.length) {
    const { oid, d } = queue.shift()!;
    if (seenQ.has(oid)) continue;
    seenQ.add(oid);
    const obj = objects.get(oid);
    if (!obj) continue;
    if (obj.type === 'commit') {
      if (depth === undefined || d < depth) {
        wantCommits.add(oid);
        for (const p of parseCommit(obj.data).parents) queue.push({ oid: p, d: d + 1 });
      }
    } else if (obj.type === 'tag') {
      queue.push({ oid: parseTag(obj.data).object, d });
    }
  }
  const newShallow: string[] = [];
  if (depth !== undefined) {
    for (const c of wantCommits) {
      const obj = objects.get(c)!;
      const parents = parseCommit(obj.data).parents;
      const beyond = parents.length === 0 || parents.some((p) => !wantCommits.has(p));
      if (beyond) newShallow.push(c);
    }
  }
  // Depth-limited object set: each wanted commit contributes itself plus its
  // own tree/blobs only — never its parents (that is what depth N means).
  // With no depth limit wantCommits already holds every reachable commit,
  // so this is exactly the full closure.
  const wantedSet = new Set<string>();
  for (const c of wantCommits) {
    wantedSet.add(c);
    const obj = objects.get(c);
    if (obj?.type === 'commit') treeObjectClosure(parseCommit(obj.data).tree, wantedSet);
  }
  for (const w of wants) {
    const obj = objects.get(w);
    if (!obj) continue;
    if (obj.type === 'tag') {
      wantedSet.add(w);
      const target = parseTag(obj.data).object;
      const tObj = objects.get(target);
      if (tObj?.type === 'commit') {
        wantedSet.add(target);
        treeObjectClosure(parseCommit(tObj.data).tree, wantedSet);
      } else if (tObj) {
        for (const oid of collectReachable([target])) wantedSet.add(oid);
      }
    } else if (obj.type !== 'commit') {
      wantedSet.add(w);
    }
  }
  // Exclusion: objects the client already has.
  const held = new Set<string>();
  for (const h of haves) {
    const obj = objects.get(h);
    if (!obj) continue;
    if (shallow.includes(h) && depth !== undefined) {
      // A shallow boundary held at the old depth: the client holds only the
      // boundary commit itself plus its own tree/blobs — never its parents.
      const o = objects.get(h);
      held.add(h);
      if (o && o.type === 'commit') treeObjectClosure(parseCommit(o.data).tree, held);
    } else {
      for (const oid of collectReachable([h])) held.add(oid);
    }
  }
  const objs: { type: ObjType; data: Buffer }[] = [];
  for (const oid of wantedSet) {
    if (held.has(oid)) continue;
    const obj = objects.get(oid);
    if (obj) objs.push({ type: obj.type, data: obj.data });
  }
  // Deterministic pack order: commits, then trees, then blobs, then tags, by oid.
  const rank = (t: ObjType): number => (t === 'commit' ? 0 : t === 'tree' ? 1 : t === 'blob' ? 2 : 3);
  objs.sort((a, b) => rank(a.type) - rank(b.type) || (hashObject(a.type, a.data) < hashObject(b.type, b.data) ? -1 : 1));
  const unshallow = shallow.filter((s) => !newShallow.includes(s) && wantCommits.has(s));
  return { objs, newShallow, unshallow };
}
function sidebandPack(pack: Buffer): Buffer {
  const parts: Buffer[] = [];
  const CHUNK = 60000;
  for (let i = 0; i < pack.length; i += CHUNK) {
    parts.push(pktLine(Buffer.concat([Buffer.from([1]), pack.subarray(i, i + CHUNK)])));
  }
  if (pack.length === 0) parts.push(pktLine(Buffer.from([1])));
  parts.push(PKT_FLUSH);
  return Buffer.concat(parts);
}

async function handleGit(req: IncomingMessage, res: ServerResponse, path: string, query: URLSearchParams, body: Buffer): Promise<boolean> {
  const m = /^\/([^/]+)\/([^/]+)\.git(\/.*)?$/.exec(path);
  if (!m) return false;
  const owner = decodePath(m[1]!);
  const repoName = decodePath(m[2]!);
  const rest = m[3] ?? '';
  const repo = getRepo(owner, repoName);
  const method = req.method ?? 'GET';

  if (method === 'GET' && rest === '/info/refs') {
    const service = query.get('service');
    if (!repo) {
      res.writeHead(404, { 'content-type': JSON_TYPE, 'x-github-request-id': nextReqId() });
      res.end(JSON.stringify({ message: 'Not Found' }));
      return true;
    }
    if (service === 'git-upload-pack') {
      const isV2 = (req.headers['git-protocol'] ?? '').includes('version=2');
      const payload = isV2 ? v2Advertisement() : v0Advertisement(repo, 'upload');
      res.writeHead(200, { 'content-type': 'application/x-git-upload-pack-advertisement', 'content-length': payload.length });
      res.end(payload);
      return true;
    }
    if (service === 'git-receive-pack') {
      const payload = v0Advertisement(repo, 'receive');
      res.writeHead(200, { 'content-type': 'application/x-git-receive-pack-advertisement', 'content-length': payload.length });
      res.end(payload);
      return true;
    }
    res.writeHead(400, { 'content-type': JSON_TYPE });
    res.end(JSON.stringify({ message: 'Bad Request' }));
    return true;
  }

  if (method === 'POST' && rest === '/git-upload-pack') {
    if (!repo) {
      res.writeHead(404, { 'content-type': JSON_TYPE, 'x-github-request-id': nextReqId() });
      res.end(JSON.stringify({ message: 'Not Found' }));
      return true;
    }
    const isV2 = (req.headers['git-protocol'] ?? '').includes('version=2');
    try {
      const result = await handleUploadPack(repo, body, isV2);
      res.writeHead(200, { 'content-type': 'application/x-git-upload-pack-result', 'content-length': result.length });
      res.end(result);
    } catch {
      res.writeHead(400, { 'content-type': JSON_TYPE });
      res.end(JSON.stringify({ message: 'Bad Request' }));
    }
    return true;
  }

  if (method === 'POST' && rest === '/git-receive-pack') {
    if (!repo) {
      res.writeHead(404, { 'content-type': JSON_TYPE, 'x-github-request-id': nextReqId() });
      res.end(JSON.stringify({ message: 'Not Found' }));
      return true;
    }
    try {
      const result = await handleReceivePack(repo, body);
      res.writeHead(200, { 'content-type': 'application/x-git-receive-pack-result', 'content-length': result.length });
      res.end(result);
    } catch {
      res.writeHead(400, { 'content-type': JSON_TYPE });
      res.end(JSON.stringify({ message: 'Bad Request' }));
    }
    return true;
  }
  return false;
}

async function handleUploadPack(repo: Repo, body: Buffer, isV2: boolean): Promise<Buffer> {
  const packets = decodePktLines(body);
  if (isV2) {
    // Parse command + args.
    let command = '';
    const args: string[] = [];
    let seenDelim = false;
    for (const p of packets) {
      if (p.kind === 'flush') break;
      if (p.kind === 'delim') { seenDelim = true; continue; }
      const line = p.payload.toString('utf8');
      if (!seenDelim) {
        if (line.startsWith('command=')) command = line.slice(8).trim();
      } else {
        args.push(line.trim());
      }
    }
    if (command === 'ls-refs') {
      const peel = args.includes('peel');
      const symrefs = args.includes('symrefs');
      const unborn = args.includes('unborn');
      const prefixes = args.filter((a) => a.startsWith('ref-prefix ')).map((a) => a.slice('ref-prefix '.length));
      const keep = (name: string): boolean => prefixes.length === 0 || prefixes.some((p) => name.startsWith(p));
      const lines: (string | 'flush')[] = [];
      const refs = sortedRefs(repo);
      const headSha = repo.refs.get(`refs/heads/${repo.defaultBranch}`);
      if (symrefs && keep('HEAD')) {
        if (headSha) lines.push(`${headSha} HEAD symref-target:refs/heads/${repo.defaultBranch}\n`);
        else if (unborn) lines.push(`unborn HEAD symref-target:refs/heads/${repo.defaultBranch}\n`);
      }
      for (const [name, oid] of refs) {
        if (!keep(name)) continue;
        let line = `${oid} ${name}`;
        if (peel) {
          const obj = objects.get(oid);
          if (name.startsWith('refs/tags/') && obj?.type === 'tag') line += ` peeled:${peelToCommit(oid)}`;
        }
        lines.push(`${line}\n`);
      }
      if (refs.length === 0 && unborn && symrefs && !keep('HEAD')) {
        // no refs at all; HEAD already emitted above only when keep('HEAD')
      }
      lines.push('flush');
      return pktLines(lines);
    }
    if (command === 'fetch') {
      const wants: string[] = [];
      const haves: string[] = [];
      const shallow: string[] = [];
      let depth: number | undefined;
      for (const a of args) {
        if (a.startsWith('want ')) wants.push(a.slice(5).trim());
        else if (a.startsWith('have ')) haves.push(a.slice(5).trim());
        else if (a.startsWith('shallow ')) shallow.push(a.slice(8).trim());
        else if (a.startsWith('deepen ')) depth = Number(a.slice(7).trim());
      }
      const cacheKey = `v2|${wants.join(',')}|${haves.join(',')}|${shallow.join(',')}|${depth ?? ''}`;
      let pack = repo.packCache.get(cacheKey);
      let newShallow: string[] = [];
      let unshallow: string[] = [];
      if (!pack) {
        const r = fetchObjects(repo, wants, haves, depth, shallow);
        newShallow = r.newShallow;
        unshallow = r.unshallow;
        pack = writePack(r.objs);
        repo.packCache.set(cacheKey, pack);
      } else {
        // Recompute shallow info cheaply for the cached pack.
        const r = fetchObjects(repo, wants, haves, depth, shallow);
        newShallow = r.newShallow;
        unshallow = r.unshallow;
      }
      const parts: Buffer[] = [];
      parts.push(pktLine('acknowledgments\n'));
      parts.push(pktLine('NAK\n'));
      parts.push(PKT_FLUSH);
      if (depth !== undefined || shallow.length > 0) {
        parts.push(pktLine('shallow-info\n'));
        for (const s of newShallow) parts.push(pktLine(`shallow ${s}\n`));
        for (const s of unshallow) parts.push(pktLine(`unshallow ${s}\n`));
        parts.push(PKT_FLUSH);
      }
      parts.push(pktLine('packfile\n'));
      parts.push(sidebandPack(pack));
      return Buffer.concat(parts);
    }
    throw new Error('unknown v2 command');
  }
  // v0 fetch: wants section, flush, haves + done, flush.
  const lines = packets.filter((p) => p.kind === 'data').map((p) => p.payload.toString('utf8').trim());
  const wants: string[] = [];
  const haves: string[] = [];
  const shallow: string[] = [];
  let depth: number | undefined;
  let inHaves = false;
  for (const line of lines) {
    if (line === '') continue;
    if (line.startsWith('want ')) {
      wants.push(line.slice(5).split(' ')[0]!);
    } else if (line.startsWith('have ')) {
      inHaves = true;
      haves.push(line.slice(5).trim());
    } else if (line.startsWith('shallow ')) shallow.push(line.slice(8).trim());
    else if (line.startsWith('deepen ')) depth = Number(line.slice(7).trim());
    else if (line === 'done') inHaves = true;
  }
  void inHaves;
  const r = fetchObjects(repo, wants, haves, depth, shallow);
  const pack = writePack(r.objs);
  const parts: Buffer[] = [];
  parts.push(pktLine('NAK\n'));
  for (const s of r.newShallow) parts.push(pktLine(`shallow ${s}\n`));
  for (const s of r.unshallow) parts.push(pktLine(`unshallow ${s}\n`));
  parts.push(sidebandPack(pack));
  return Buffer.concat(parts);
}

async function handleReceivePack(repo: Repo, body: Buffer): Promise<Buffer> {
  // Split pkt-line command section from the pack: decode until the flush packet.
  let off = 0;
  const commands: { oldOid: string; newOid: string; ref: string }[] = [];
  let packStart = body.length;
  while (off < body.length) {
    if (off + 4 > body.length) break;
    const len = parseInt(body.subarray(off, off + 4).toString('ascii'), 16);
    if (Number.isNaN(len)) break;
    if (len === 0) {
      off += 4;
      packStart = off;
      break;
    }
    const payload = body.subarray(off + 4, off + len).toString('utf8');
    const nul = payload.indexOf('\0');
    const cmdLine = (nul >= 0 ? payload.slice(0, nul) : payload).trim();
    const parts = cmdLine.split(' ');
    if (parts.length >= 3) commands.push({ oldOid: parts[0]!, newOid: parts[1]!, ref: parts[2]! });
    off += len;
  }
  const packBytes = body.subarray(packStart);
  let unpackOk = true;
  let unpackDetail = 'ok';
  if (packBytes.length > 0) {
    try {
      const objs = readPack(packBytes);
      for (const o of objs) putObject(o.type, o.data);
    } catch (err) {
      unpackOk = false;
      unpackDetail = err instanceof Error ? err.message : 'unpack error';
    }
  }
  const reportLines: string[] = [];
  reportLines.push(`unpack ${unpackOk ? 'ok' : unpackDetail}\n`);
  for (const cmd of commands) {
    let ok = false;
    let reason = '';
    if (unpackOk) {
      const current = repo.refs.get(cmd.ref);
      if (cmd.newOid === ZERO_OID) {
        // delete
        if (!current) { ok = false; reason = 'no such ref'; }
        else if (current !== cmd.oldOid && cmd.oldOid !== ZERO_OID) { ok = false; reason = 'non-fast-forward'; }
        else { repo.refs.delete(cmd.ref); ok = true; }
      } else if (cmd.oldOid === ZERO_OID) {
        if (current !== undefined) { ok = false; reason = 'already exists'; }
        else if (!objects.has(cmd.newOid)) { ok = false; reason = 'object not found'; }
        else { repo.refs.set(cmd.ref, cmd.newOid); ok = true; }
      } else {
        if (current !== cmd.oldOid) { ok = false; reason = 'non-fast-forward'; }
        else if (!objects.has(cmd.newOid)) { ok = false; reason = 'object not found'; }
        else if (!isAncestor(cmd.oldOid, cmd.newOid)) { ok = false; reason = 'non-fast-forward'; }
        else { repo.refs.set(cmd.ref, cmd.newOid); ok = true; }
      }
    } else {
      reason = 'unpack failed';
    }
    reportLines.push(ok ? `ok ${cmd.ref}\n` : `ng ${cmd.ref} ${reason}\n`);
  }
  repo.packCache.clear();
  const inner = pktLines([...reportLines, 'flush']);
  const band1 = pktLine(Buffer.concat([Buffer.from([1]), inner]));
  return Buffer.concat([band1, PKT_FLUSH]);
}

// ------------------------------------------------------------------ GraphQL v4

const GQL_ISSUE_FIELDS = ['id', 'number', 'title', 'state', 'body', 'url', 'createdAt', 'updatedAt', 'author', 'comments', 'labels', 'closed', 'closedAt', 'repository'];
const GQL_PR_FIELDS = ['id', 'number', 'title', 'state', 'merged', 'headRefName', 'baseRefName', 'mergeable', 'files', 'body', 'createdAt', 'updatedAt', 'author', 'headRef', 'baseRef', 'mergeCommit', 'closed', 'repository', 'url'];
const GQL_REPO_FIELDS = ['id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission', 'hasIssuesEnabled', 'issue', 'issues', 'pullRequests', 'pullRequest', 'url', 'description', 'createdAt', 'updatedAt', 'isPrivate', 'fork'];
const GQL_SEARCH_ENUM = ['ISSUE', 'REPOSITORY', 'USER', 'DISCUSSION', 'CODE', 'COMMIT'];

interface GqlField { name: string; alias: string | null; args: Record<string, unknown>; selections: GqlField[]; fragments: string[] }
interface GqlOp { kind: 'query' | 'mutation'; name: string | null; varDefs: Record<string, string>; selections: GqlField[] }

function gqlTokenize(src: string): string[] {
  const tokens: string[] = [];
  let i = 0;
  while (i < src.length) {
    const c = src[i]!;
    if (c === ' ' || c === '\n' || c === '\r' || c === '\t' || c === ',') { i++; continue; }
    if (c === '#') { while (i < src.length && src[i] !== '\n') i++; continue; }
    if (c === '.' && src.slice(i, i + 3) === '...') { tokens.push('...'); i += 3; continue; }
    if ('{}():!=$@[]'.includes(c)) { tokens.push(c); i++; continue; }
    if (c === '"') {
      let j = i + 1;
      let s = '';
      while (j < src.length && src[j] !== '"') {
        if (src[j] === '\\' && j + 1 < src.length) { s += src[j + 1]; j += 2; }
        else { s += src[j]; j++; }
      }
      tokens.push(JSON.stringify(s));
      i = j + 1;
      continue;
    }
    if (/[0-9-]/.test(c) && (c !== '-' || /[0-9]/.test(src[i + 1] ?? ''))) {
      let j = i;
      while (j < src.length && /[0-9.]/.test(src[j]!)) j++;
      tokens.push(src.slice(i, j));
      i = j;
      continue;
    }
    if (/[A-Za-z_]/.test(c)) {
      let j = i;
      while (j < src.length && /[A-Za-z0-9_]/.test(src[j]!)) j++;
      tokens.push(src.slice(i, j));
      i = j;
      continue;
    }
    i++;
  }
  return tokens;
}
class GqlParser {
  private pos = 0;
  private tokens: string[];
  private vars: Record<string, unknown>;
  constructor(tokens: string[], vars: Record<string, unknown>) {
    this.tokens = tokens;
    this.vars = vars;
  }
  private peek(): string | undefined { return this.tokens[this.pos]; }
  private next(): string { return this.tokens[this.pos++]!; }
  private expect(t: string): void { if (this.next() !== t) throw new Error(`expected ${t}`); }
  parseOperation(): GqlOp {
    let kind: 'query' | 'mutation' = 'query';
    let name: string | null = null;
    const varDefs: Record<string, string> = {};
    const t = this.peek();
    if (t === 'query' || t === 'mutation') {
      kind = this.next() as 'query' | 'mutation';
      const n = this.peek();
      if (n !== undefined && n !== '{' && n !== '(') name = this.next();
    }
    if (this.peek() === '(') {
      this.next();
      while (this.peek() !== ')') {
        this.expect('$');
        const vname = this.next();
        this.expect(':');
        let type = '';
        while (this.peek() !== ')' && this.peek() !== '$' && this.peek() !== '=' && this.peek() !== ',') type += this.next();
        varDefs[vname] = type;
        if (this.peek() === '=') { this.next(); this.parseValue(); }
      }
      this.expect(')');
    }
    const selections = this.parseSelectionSet();
    return { kind, name, varDefs, selections };
  }
  parseSelectionSet(): GqlField[] {
    this.expect('{');
    const fields: GqlField[] = [];
    while (this.peek() !== '}') {
      if (this.peek() === '...') {
        this.next();
        if (this.peek() === 'on') {
          this.next();
          const typeName = this.next();
          const sub = this.parseSelectionSet();
          fields.push({ name: `...on:${typeName}`, alias: null, args: {}, selections: sub, fragments: [] });
        } else {
          const fragName = this.next();
          fields.push({ name: `...${fragName}`, alias: null, args: {}, selections: [], fragments: [] });
        }
        continue;
      }
      let name = this.next();
      let alias: string | null = null;
      if (this.peek() === ':') {
        this.next();
        alias = name;
        name = this.next();
      }
      let args: Record<string, unknown> = {};
      if (this.peek() === '(') {
        this.next();
        while (this.peek() !== ')') {
          const argName = this.next();
          this.expect(':');
          args[argName] = this.parseValue();
        }
        this.expect(')');
      }
      let selections: GqlField[] = [];
      if (this.peek() === '{') selections = this.parseSelectionSet();
      fields.push({ name, alias, args, selections, fragments: [] });
    }
    this.expect('}');
    return fields;
  }
  parseValue(): unknown {
    const t = this.next();
    if (t === '$') return this.vars[this.next()!] ?? null;
    if (t === '{') {
      const obj: Record<string, unknown> = {};
      while (this.peek() !== '}') {
        const k = this.next();
        this.expect(':');
        obj[k] = this.parseValue();
      }
      this.expect('}');
      return obj;
    }
    if (t === '[') {
      const arr: unknown[] = [];
      while (this.peek() !== ']') arr.push(this.parseValue());
      this.expect(']');
      return arr;
    }
    if (t === 'true') return true;
    if (t === 'false') return false;
    if (t === 'null') return null;
    if (t.startsWith('"')) return JSON.parse(t) as string;
    const n = Number(t);
    if (!Number.isNaN(n)) return n;
    return t; // enum
  }
}

type GqlCtx = { repo: Repo | null; issue: Issue | null; pr: Pull | null; typeName: string };
function gqlState(issue: Issue): string {
  return issue.state === 'open' ? 'OPEN' : 'CLOSED';
}
function gqlPrState(pr: Pull): string {
  if (pr.merged) return 'MERGED';
  return pr.state === 'open' ? 'OPEN' : 'CLOSED';
}
function resolveIssueFields(repo: Repo, issue: Issue, fields: GqlField[], errors: string[]): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const f of fields) {
    const key = f.alias ?? f.name;
    switch (f.name) {
      case 'id': out[key] = gid('issue', `${VIEWER_LOGIN}/${repo.name}:${issue.number}`); break;
      case 'number': out[key] = issue.number; break;
      case 'title': out[key] = issue.title; break;
      case 'state': out[key] = gqlState(issue); break;
      case 'body': out[key] = issue.body; break;
      case 'url': out[key] = ''; break;
      case 'createdAt': out[key] = issue.createdAt; break;
      case 'updatedAt': out[key] = issue.updatedAt; break;
      default:
        if (!GQL_ISSUE_FIELDS.includes(f.name)) errors.push(`Cannot query field "${f.name}" on type "Issue".`);
        else out[key] = null;
    }
  }
  return out;
}
function resolvePrFields(repo: Repo, pr: Pull, fields: GqlField[], errors: string[]): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const f of fields) {
    const key = f.alias ?? f.name;
    switch (f.name) {
      case 'id': out[key] = gid('pr', `${VIEWER_LOGIN}/${repo.name}:${pr.number}`); break;
      case 'number': out[key] = pr.number; break;
      case 'title': out[key] = pr.title; break;
      case 'state': out[key] = gqlPrState(pr); break;
      case 'merged': out[key] = pr.merged; break;
      case 'headRefName': out[key] = pr.headBranch; break;
      case 'baseRefName': out[key] = pr.baseBranch; break;
      case 'mergeable': out[key] = prMergeable(repo, pr); break;
      case 'files': {
        const first = typeof f.args.first === 'number' ? f.args.first : 100;
        const files = prFiles(repo, pr).slice(0, first);
        out[key] = { nodes: files.map((fl) => ({ path: fl.filename })), totalCount: files.length };
        break;
      }
      default:
        if (!GQL_PR_FIELDS.includes(f.name)) errors.push(`Cannot query field "${f.name}" on type "PullRequest".`);
        else out[key] = null;
    }
  }
  return out;
}
function resolveRepoFields(repo: Repo, fields: GqlField[], errors: string[]): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const f of fields) {
    const key = f.alias ?? f.name;
    switch (f.name) {
      case 'id': out[key] = gid('repo', `${VIEWER_LOGIN}/${repo.name}`); break;
      case 'databaseId': out[key] = repo.id; break;
      case 'name': out[key] = repo.name; break;
      case 'nameWithOwner': out[key] = `${VIEWER_LOGIN}/${repo.name}`; break;
      case 'owner': out[key] = { login: VIEWER_LOGIN, id: VIEWER_ID }; break;
      case 'defaultBranchRef': {
        const sha = repo.refs.get(`refs/heads/${repo.defaultBranch}`);
        out[key] = sha ? { name: repo.defaultBranch } : null;
        break;
      }
      case 'viewerPermission': out[key] = 'ADMIN'; break;
      case 'hasIssuesEnabled': out[key] = true; break;
      case 'issue': {
        const num = f.args.number as number;
        const issue = repo.issues.get(num);
        out[key] = issue ? resolveIssueFields(repo, issue, f.selections, errors) : null;
        break;
      }
      case 'issues': {
        let list = [...repo.issues.values()];
        const states = f.args.states as string[] | undefined;
        if (states) list = list.filter((i) => states.includes(gqlState(i)));
        const orderBy = f.args.orderBy as { field?: string; direction?: string } | undefined;
        if (orderBy?.field === 'CREATED_AT') {
          const desc = orderBy.direction !== 'ASC';
          list.sort((a, b) => (desc ? b.seq - a.seq || b.number - a.number : a.seq - b.seq || a.number - b.number));
        } else {
          list.sort((a, b) => a.number - b.number);
        }
        const after = f.args.after as string | undefined;
        if (after) {
          const cursorNum = decodeCursor(after);
          if (cursorNum !== null) {
            const idx = list.findIndex((i) => i.number === cursorNum);
            if (idx >= 0) list = list.slice(idx + 1);
          }
        }
        const first = typeof f.args.first === 'number' ? f.args.first : list.length;
        const page = list.slice(0, first);
        const nodes: Record<string, unknown>[] = [];
        const pageInfoSel = f.selections.find((s) => s.name === 'pageInfo');
        const nodeSel = f.selections.find((s) => s.name === 'nodes')?.selections ?? [];
        void pageInfoSel;
        for (const issue of page) nodes.push(resolveIssueFields(repo, issue, nodeSel, errors));
        const hasNext = list.length > first;
        const endCursor = page.length ? encodeCursor(page[page.length - 1]!.number) : null;
        out[key] = { nodes, pageInfo: { hasNextPage: hasNext, endCursor }, totalCount: list.length };
        break;
      }
      case 'pullRequests': {
        const first = typeof f.args.first === 'number' ? f.args.first : 100;
        const list = [...repo.pulls.values()].sort((a, b) => b.seq - a.seq).slice(0, first);
        const nodeSel = f.selections.find((s) => s.name === 'nodes')?.selections ?? [];
        out[key] = { nodes: list.map((p) => resolvePrFields(repo, p, nodeSel, errors)), totalCount: repo.pulls.size };
        break;
      }
      case 'pullRequest': {
        const num = f.args.number as number;
        const pr = repo.pulls.get(num);
        out[key] = pr ? resolvePrFields(repo, pr, f.selections, errors) : null;
        break;
      }
      default:
        if (!GQL_REPO_FIELDS.includes(f.name)) errors.push(`Cannot query field "${f.name}" on type "Repository".`);
        else out[key] = null;
    }
  }
  return out;
}
function encodeCursor(n: number): string {
  return Buffer.from(`cursor:${n}`, 'utf8').toString('base64');
}
function decodeCursor(c: string): number | null {
  try {
    const t = Buffer.from(c, 'base64').toString('utf8');
    const m = /^cursor:(\d+)$/.exec(t);
    return m ? Number(m[1]) : null;
  } catch {
    return null;
  }
}
function typeFields(name: string): string[] {
  if (name === 'Issue') return GQL_ISSUE_FIELDS;
  if (name === 'PullRequest') return GQL_PR_FIELDS;
  if (name === 'Repository') return GQL_REPO_FIELDS;
  return [];
}
function executeGql(query: string, variables: Record<string, unknown>): { data: Record<string, unknown> | null; errors: { message: string }[] } {
  const errors: string[] = [];
  let op: GqlOp;
  try {
    const parser = new GqlParser(gqlTokenize(query), variables);
    op = parser.parseOperation();
  } catch (err) {
    return { data: null, errors: [{ message: err instanceof Error ? err.message : 'Parse error' }] };
  }
  const data: Record<string, unknown> = {};
  const resolveTop = (fields: GqlField[]): void => {
    for (const f of fields) {
      const key = f.alias ?? f.name;
      if (op.kind === 'query') {
        switch (f.name) {
          case 'viewer': {
            const v: Record<string, unknown> = {};
            const allowed = ['login', 'id', 'name', 'email', 'avatarUrl', 'url'];
            for (const sub of f.selections) {
              if (sub.name === 'login') v.login = VIEWER_LOGIN;
              else if (sub.name === 'id') v.id = VIEWER_ID;
              else if (sub.name === 'name') v.name = VIEWER_NAME;
              else if (sub.name === 'email') v.email = VIEWER_EMAIL;
              else if (sub.name === 'avatarUrl' || sub.name === 'url') v[sub.name] = '';
              else errors.push(`Cannot query field "${sub.name}" on type "User".`);
              void allowed;
            }
            if (f.selections.length === 0) { v.login = VIEWER_LOGIN; v.id = VIEWER_ID; }
            data[key] = v;
            break;
          }
          case 'repository': {
            const owner = String(f.args.owner ?? '');
            const name = String(f.args.name ?? '');
            const repo = getRepo(owner, name);
            data[key] = repo ? resolveRepoFields(repo, f.selections, errors) : null;
            break;
          }
          case 'node': {
            const decoded = decodeGid(String(f.args.id ?? ''));
            if (decoded?.kind === 'repo') {
              const [owner, name] = decoded.key.split('/');
              const repo = owner && name ? getRepo(owner, name) : undefined;
              if (repo) {
                const obj: Record<string, unknown> = { id: String(f.args.id), __typename: 'Repository' };
                for (const sub of f.selections) {
                  if (sub.name.startsWith('...on:')) {
                    if (sub.name === '...on:Repository') {
                      const r = resolveRepoFields(repo, sub.selections, errors);
                      Object.assign(obj, r);
                    }
                  } else if (sub.name === 'id') obj.id = gid('repo', `${VIEWER_LOGIN}/${repo.name}`);
                  else if (sub.name === 'name') obj.name = repo.name;
                  else if (sub.name === 'nameWithOwner') obj.nameWithOwner = `${VIEWER_LOGIN}/${repo.name}`;
                  else errors.push(`Cannot query field "${sub.name}" on type "Node".`);
                }
                data[key] = obj;
              } else data[key] = null;
            } else data[key] = null;
            break;
          }
          case '__type': {
            const name = String(f.args.name ?? '');
            const fieldSel = f.selections.find((s) => s.name === 'fields');
            const enumSel = f.selections.find((s) => s.name === 'enumValues');
            const out: Record<string, unknown> = { name };
            if (fieldSel) out.fields = typeFields(name).map((n) => ({ name: n }));
            if (enumSel) out.enumValues = name === 'SearchType' ? GQL_SEARCH_ENUM.map((n) => ({ name: n })) : [];
            data[key] = out;
            break;
          }
          default:
            errors.push(`Cannot query field "${f.name}" on type "Query".`);
            data[key] = null;
        }
      } else {
        // mutations
        switch (f.name) {
          case 'createIssue': {
            const input = (f.args.input ?? {}) as Record<string, unknown>;
            const decoded = decodeGid(String(input.repositoryId ?? ''));
            let repo: Repo | undefined;
            if (decoded?.kind === 'repo') {
              const [owner, name] = decoded.key.split('/');
              repo = owner && name ? getRepo(owner, name) : undefined;
            }
            if (!repo) { errors.push('Repository not found'); data[key] = { issue: null }; break; }
            const number = repo.nextNumber++;
            const sec = tick();
            const issue: Issue = { id: nextEntityId(), number, title: String(input.title ?? ''), body: String(input.body ?? ''), state: 'open', labels: [], seq: number, createdAt: isoOf(sec), updatedAt: isoOf(sec), comments: [] };
            repo.issues.set(number, issue);
            data[key] = { issue: resolveIssueFields(repo, issue, f.selections.find((s) => s.name === 'issue')?.selections ?? [], errors) };
            break;
          }
          case 'closeIssue': {
            const input = (f.args.input ?? {}) as Record<string, unknown>;
            const decoded = decodeGid(String(input.issueId ?? ''));
            let found: { repo: Repo; issue: Issue } | null = null;
            if (decoded?.kind === 'issue') {
              const m = /^([^/]+)\/([^:]+):(\d+)$/.exec(decoded.key);
              if (m) {
                const repo = getRepo(m[1]!, m[2]!);
                const issue = repo?.issues.get(Number(m[3]));
                if (repo && issue) found = { repo, issue };
              }
            }
            if (!found) { errors.push('Issue not found'); data[key] = { issue: null }; break; }
            found.issue.state = 'closed';
            found.issue.updatedAt = nowIso();
            data[key] = { issue: resolveIssueFields(found.repo, found.issue, f.selections.find((s) => s.name === 'issue')?.selections ?? [], errors) };
            break;
          }
          case 'createPullRequest': {
            const input = (f.args.input ?? {}) as Record<string, unknown>;
            const decoded = decodeGid(String(input.repositoryId ?? ''));
            let repo: Repo | undefined;
            if (decoded?.kind === 'repo') {
              const [owner, name] = decoded.key.split('/');
              repo = owner && name ? getRepo(owner, name) : undefined;
            }
            if (!repo) { errors.push('Repository not found'); data[key] = { pullRequest: null }; break; }
            const headBranch = String(input.headRefName ?? '');
            const baseBranch = String(input.baseRefName ?? repo.defaultBranch);
            const headSha = repo.refs.get(`refs/heads/${headBranch}`);
            const baseSha = repo.refs.get(`refs/heads/${baseBranch}`);
            if (!headSha || !baseSha) { errors.push('Head or base branch not found'); data[key] = { pullRequest: null }; break; }
            let pr = [...repo.pulls.values()].find((p) => p.state === 'open' && p.headBranch === headBranch && p.baseBranch === baseBranch);
            if (!pr) {
              const number = repo.nextNumber++;
              const sec = tick();
              pr = { id: nextEntityId(), number, title: String(input.title ?? ''), body: String(input.body ?? ''), state: 'open', merged: false, mergedAt: null, mergeCommitSha: null, headBranch, baseBranch, headSha, baseSha, seq: number, createdAt: isoOf(sec) };
              repo.pulls.set(number, pr);
            }
            data[key] = { pullRequest: resolvePrFields(repo, pr, f.selections.find((s) => s.name === 'pullRequest')?.selections ?? [], errors) };
            break;
          }
          case 'mergePullRequest': {
            const input = (f.args.input ?? {}) as Record<string, unknown>;
            const decoded = decodeGid(String(input.pullRequestId ?? ''));
            let found: { repo: Repo; pr: Pull } | null = null;
            if (decoded?.kind === 'pr') {
              const m = /^([^/]+)\/([^:]+):(\d+)$/.exec(decoded.key);
              if (m) {
                const repo = getRepo(m[1]!, m[2]!);
                const pr = repo?.pulls.get(Number(m[3]));
                if (repo && pr) found = { repo, pr };
              }
            }
            if (!found) { errors.push('Pull request not found'); data[key] = { pullRequest: null }; break; }
            const { repo, pr } = found;
            if (pr.state === 'open' && !prConflicts(repo, pr)) {
              const headFlat = flattenTree(parseCommit(objects.get(pr.headSha)!.data).tree);
              const baseTip = repo.refs.get(`refs/heads/${pr.baseBranch}`);
              const mergeSha = makeCommit(repo, baseTip, headFlat, `Merge pull request #${pr.number} from ${pr.headBranch}\n\n${pr.title}`);
              repo.refs.set(`refs/heads/${pr.baseBranch}`, mergeSha);
              pr.state = 'closed';
              pr.merged = true;
              pr.mergedAt = nowIso();
              pr.mergeCommitSha = mergeSha;
              pr.baseSha = mergeSha;
            }
            data[key] = { pullRequest: resolvePrFields(repo, pr, f.selections.find((s) => s.name === 'pullRequest')?.selections ?? [], errors) };
            break;
          }
          case 'addComment': {
            const input = (f.args.input ?? {}) as Record<string, unknown>;
            const decoded = decodeGid(String(input.subjectId ?? ''));
            let found: { repo: Repo; issue: Issue } | null = null;
            if (decoded?.kind === 'issue') {
              const m = /^([^/]+)\/([^:]+):(\d+)$/.exec(decoded.key);
              if (m) {
                const repo = getRepo(m[1]!, m[2]!);
                const issue = repo?.issues.get(Number(m[3]));
                if (repo && issue) found = { repo, issue };
              }
            }
            if (!found) { errors.push('Subject not found'); data[key] = { commentEdge: null }; break; }
            const comment = { id: nextEntityId(), body: String(input.body ?? ''), createdAt: nowIso() };
            found.issue.comments.push(comment);
            data[key] = { commentEdge: { node: { id: gid('comment', `${VIEWER_LOGIN}/${found.repo.name}:${found.issue.number}:${comment.id}`), body: comment.body } } };
            break;
          }
          default:
            errors.push(`Cannot query field "${f.name}" on type "Mutation".`);
            data[key] = null;
        }
      }
    }
  };
  resolveTop(op.selections);
  return { data, errors: errors.map((message) => ({ message })) };
}

async function handleGraphql(req: IncomingMessage, res: ServerResponse, body: Buffer): Promise<void> {
  if (!isAuthed(req)) {
    sendErr(res, 401, 'Bad credentials');
    return;
  }
  const parsed = parseJsonBody(body);
  if (!parsed.ok) {
    sendJson(res, 400, { errors: [{ message: 'Problems parsing JSON' }] });
    return;
  }
  const runOne = (op: unknown): Record<string, unknown> => {
    if (typeof op !== 'object' || op === null) return { data: null, errors: [{ message: 'Invalid operation' }] };
    const o = op as Record<string, unknown>;
    const query = String(o.query ?? '');
    const variables = (o.variables ?? {}) as Record<string, unknown>;
    const result = executeGql(query, variables);
    return { data: result.data, ...(result.errors.length ? { errors: result.errors } : {}) };
  };
  if (Array.isArray(parsed.value)) {
    sendJson(res, 200, parsed.value.map(runOne));
  } else {
    sendJson(res, 200, runOne(parsed.value));
  }
}

// ------------------------------------------------------------------ MCP

const MCP_TOOLS: Record<string, unknown>[] = [
  { name: 'create_repo', description: 'Create a new repository for the authenticated user.', inputSchema: { type: 'object', properties: { name: { type: 'string', description: 'Repository name' }, description: { type: 'string', description: 'Repository description' }, private: { type: 'boolean', description: 'Whether the repository is private' } }, required: ['name'] } },
  { name: 'get_repo', description: 'Get a repository by owner and name.', inputSchema: { type: 'object', properties: { owner: { type: 'string', description: 'Repository owner login' }, repo: { type: 'string', description: 'Repository name' }, full_name: { type: 'string', description: 'Full repository name owner/repo' } }, required: ['owner', 'repo'] } },
  { name: 'create_issue', description: 'Create an issue in a repository.', inputSchema: { type: 'object', properties: { owner: { type: 'string', description: 'Repository owner login' }, repo: { type: 'string', description: 'Repository name' }, title: { type: 'string', description: 'Issue title' }, body: { type: 'string', description: 'Issue body' } }, required: ['owner', 'repo', 'title'] } },
  { name: 'list_issues', description: 'List issues in a repository.', inputSchema: { type: 'object', properties: { owner: { type: 'string', description: 'Repository owner login' }, repo: { type: 'string', description: 'Repository name' }, state: { type: 'string', description: 'Issue state: open, closed or all' } }, required: ['owner', 'repo'] } },
  { name: 'create_pull_request', description: 'Create a pull request from a head branch into a base branch.', inputSchema: { type: 'object', properties: { owner: { type: 'string', description: 'Repository owner login' }, repo: { type: 'string', description: 'Repository name' }, title: { type: 'string', description: 'Pull request title' }, body: { type: 'string', description: 'Pull request body' }, head: { type: 'string', description: 'Head branch name' }, base: { type: 'string', description: 'Base branch name' } }, required: ['owner', 'repo', 'title', 'head', 'base'] } },
  { name: 'merge_pull_request', description: 'Merge a pull request by number.', inputSchema: { type: 'object', properties: { owner: { type: 'string', description: 'Repository owner login' }, repo: { type: 'string', description: 'Repository name' }, pull_number: { type: 'number', description: 'Pull request number' } }, required: ['owner', 'repo', 'pull_number'] } },
];
const MCP_TOOL_REQUIRED: Record<string, string[]> = {
  create_repo: ['name'],
  get_repo: ['owner', 'repo'],
  create_issue: ['owner', 'repo', 'title'],
  list_issues: ['owner', 'repo'],
  create_pull_request: ['owner', 'repo', 'title', 'head', 'base'],
  merge_pull_request: ['owner', 'repo', 'pull_number'],
};
function mcpTextResult(value: unknown): Record<string, unknown> {
  const text = typeof value === 'string' ? value : JSON.stringify(value);
  return { content: [{ type: 'text', text }], structuredContent: value };
}
function mcpErrorResult(message: string): Record<string, unknown> {
  return { content: [{ type: 'text', text: message }], isError: true };
}
function resolveOwnerRepo(args: Record<string, unknown>): { owner: string; name: string } {
  let owner = typeof args.owner === 'string' ? args.owner : '';
  let name = typeof args.repo === 'string' ? args.repo : '';
  const full = typeof args.full_name === 'string' ? args.full_name : '';
  if ((!owner || !name) && full.includes('/')) {
    const [o, n] = full.split('/', 2);
    if (!owner) owner = o ?? '';
    if (!name) name = n ?? '';
  }
  if (!name && typeof args.name === 'string') name = args.name;
  return { owner, name };
}
function mcpCallTool(name: string, args: Record<string, unknown>): Record<string, unknown> {
  const required = MCP_TOOL_REQUIRED[name];
  if (!required) return mcpErrorResult(`Unknown tool: ${name}`);
  const missing = required.filter((k) => args[k] === undefined || args[k] === null || args[k] === '');
  if (missing.length) return mcpErrorResult(`Missing required argument: ${missing.join(', ')}`);
  switch (name) {
    case 'create_repo': {
      const repoName = String(args.name);
      if (repos.has(repoName)) return mcpErrorResult('Repository already exists');
      const repo = createRepoRecord(repoName, args.private === true, typeof args.description === 'string' ? args.description : '');
      return mcpTextResult({ id: repo.id, name: repo.name, full_name: `${VIEWER_LOGIN}/${repo.name}`, owner: { login: VIEWER_LOGIN }, private: repo.isPrivate, default_branch: repo.defaultBranch, description: repo.description });
    }
    case 'get_repo': {
      const { owner, name } = resolveOwnerRepo(args);
      const repo = getRepo(owner, name);
      if (!repo) return mcpErrorResult('Repository not found');
      return mcpTextResult({ id: repo.id, name: repo.name, full_name: `${owner}/${repo.name}`, owner: { login: owner }, private: repo.isPrivate, default_branch: repo.defaultBranch, description: repo.description });
    }
    case 'create_issue': {
      const { owner, name } = resolveOwnerRepo(args);
      const repo = getRepo(owner, name);
      if (!repo) return mcpErrorResult('Repository not found');
      const number = repo.nextNumber++;
      const sec = tick();
      const issue: Issue = { id: nextEntityId(), number, title: String(args.title), body: typeof args.body === 'string' ? args.body : '', state: 'open', labels: [], seq: number, createdAt: isoOf(sec), updatedAt: isoOf(sec), comments: [] };
      repo.issues.set(number, issue);
      return mcpTextResult({ id: issue.id, number, title: issue.title, body: issue.body, state: 'open', repository: `${owner}/${name}` });
    }
    case 'list_issues': {
      const { owner, name } = resolveOwnerRepo(args);
      const repo = getRepo(owner, name);
      if (!repo) return mcpErrorResult('Repository not found');
      const state = typeof args.state === 'string' ? args.state : 'open';
      let list = [...repo.issues.values()];
      if (state !== 'all') list = list.filter((i) => i.state === state);
      list.sort((a, b) => b.seq - a.seq);
      return mcpTextResult({ issues: list.map((i) => ({ id: i.id, number: i.number, title: i.title, body: i.body, state: i.state })) });
    }
    case 'create_pull_request': {
      const { owner, name } = resolveOwnerRepo(args);
      const repo = getRepo(owner, name);
      if (!repo) return mcpErrorResult('Repository not found');
      const headBranch = String(args.head);
      const baseBranch = String(args.base);
      const headSha = repo.refs.get(`refs/heads/${headBranch}`);
      const baseSha = repo.refs.get(`refs/heads/${baseBranch}`);
      if (!headSha || !baseSha) return mcpErrorResult('Head or base branch not found');
      let pr = [...repo.pulls.values()].find((p) => p.state === 'open' && p.headBranch === headBranch && p.baseBranch === baseBranch);
      if (!pr) {
        const number = repo.nextNumber++;
        const sec = tick();
        pr = { id: nextEntityId(), number, title: String(args.title), body: typeof args.body === 'string' ? args.body : '', state: 'open', merged: false, mergedAt: null, mergeCommitSha: null, headBranch, baseBranch, headSha, baseSha, seq: number, createdAt: isoOf(sec) };
        repo.pulls.set(number, pr);
      }
      return mcpTextResult({ id: pr.id, number: pr.number, title: pr.title, state: 'open', head: { ref: pr.headBranch, sha: pr.headSha }, base: { ref: pr.baseBranch, sha: pr.baseSha } });
    }
    case 'merge_pull_request': {
      const { owner, name } = resolveOwnerRepo(args);
      const repo = getRepo(owner, name);
      if (!repo) return mcpErrorResult('Repository not found');
      const num = Number(args.pull_number);
      const pr = repo.pulls.get(num);
      if (!pr) return mcpErrorResult('Pull request not found');
      if (pr.state !== 'open') return mcpErrorResult('Pull request is not open');
      if (prConflicts(repo, pr)) return mcpErrorResult('Merge conflict');
      const headFlat = flattenTree(parseCommit(objects.get(pr.headSha)!.data).tree);
      const baseTip = repo.refs.get(`refs/heads/${pr.baseBranch}`);
      const mergeSha = makeCommit(repo, baseTip, headFlat, `Merge pull request #${pr.number} from ${pr.headBranch}\n\n${pr.title}`);
      repo.refs.set(`refs/heads/${pr.baseBranch}`, mergeSha);
      pr.state = 'closed';
      pr.merged = true;
      pr.mergedAt = nowIso();
      pr.mergeCommitSha = mergeSha;
      pr.baseSha = mergeSha;
      return mcpTextResult({ number: pr.number, merged: true, sha: mergeSha, state: 'closed' });
    }
    default:
      return mcpErrorResult(`Unknown tool: ${name}`);
  }
}

async function handleMcp(req: IncomingMessage, res: ServerResponse, body: Buffer): Promise<void> {
  const sendRpc = (status: number, payload: unknown): void => {
    const text = `${JSON.stringify(payload)}\n`;
    res.writeHead(status, { 'content-type': JSON_TYPE, 'content-length': Buffer.byteLength(text), 'x-github-request-id': nextReqId() });
    res.end(text);
  };
  if ((req.method ?? 'GET') === 'DELETE') {
    sendRpc(200, {});
    return;
  }
  if (!isAuthed(req)) {
    sendRpc(401, { jsonrpc: '2.0', id: null, error: { code: -32001, message: 'Unauthorized' } });
    return;
  }
  const text = body.toString('utf8');
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    sendRpc(400, { jsonrpc: '2.0', id: null, error: { code: -32700, message: 'Parse error' } });
    return;
  }
  if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
    sendRpc(400, { jsonrpc: '2.0', id: null, error: { code: -32600, message: 'Invalid Request' } });
    return;
  }
  const msg = parsed as Record<string, unknown>;
  const id = (msg.id ?? null) as string | number | null;
  const method = String(msg.method ?? '');
  const params = (msg.params ?? {}) as Record<string, unknown>;
  const isNotification = msg.id === undefined;
  if (isNotification) {
    sendRpc(202, {});
    return;
  }
  if (method === 'server/discover') {
    sendRpc(200, {
      jsonrpc: '2.0', id,
      result: {
        resultType: 'discover',
        supportedVersions: ['2026-07-28', '2025-11-25', '2025-06-18'],
        capabilities: { tools: {} },
        serverInfo: { name: 'github-core-clone', version: '1.0.0' },
        _meta: { 'io.modelcontextprotocol/serverInfo': { name: 'github-core-clone', version: '1.0.0' } },
      },
    });
    return;
  }
  if (method === 'initialize') {
    const requested = String(params.protocolVersion ?? '2025-11-25');
    const negotiated = ['2026-07-28', '2025-11-25', '2025-06-18', '2025-03-26', '2024-11-05'].includes(requested) ? requested : '2025-11-25';
    sendRpc(200, {
      jsonrpc: '2.0', id,
      result: { protocolVersion: negotiated, capabilities: { tools: {} }, serverInfo: { name: 'github-core-clone', version: '1.0.0' } },
    });
    return;
  }
  if (method === 'tools/list') {
    sendRpc(200, { jsonrpc: '2.0', id, result: { tools: MCP_TOOLS } });
    return;
  }
  if (method === 'tools/call') {
    const toolName = String(params.name ?? '');
    const args = (params.arguments ?? {}) as Record<string, unknown>;
    const known = MCP_TOOLS.some((t) => t.name === toolName);
    if (!known) {
      sendRpc(200, { jsonrpc: '2.0', id, error: { code: -32602, message: `Unknown tool: ${toolName}` } });
      return;
    }
    const result = mcpCallTool(toolName, args);
    sendRpc(200, { jsonrpc: '2.0', id, result });
    return;
  }
  if (method === 'ping') {
    sendRpc(200, { jsonrpc: '2.0', id, result: {} });
    return;
  }
  sendRpc(200, { jsonrpc: '2.0', id, error: { code: -32601, message: `Method not found: ${method}` } });
}

// ------------------------------------------------------------------ router

function pathOf(req: IncomingMessage): string {
  try {
    return new URL(req.url ?? '/', 'http://entry').pathname;
  } catch {
    return '/';
  }
}

export function createApp(): Server {
  return createServer((req: IncomingMessage, res: ServerResponse) => {
    void (async () => {
      try {
        const path = pathOf(req);
        const method = req.method ?? 'GET';
        const url = new URL(req.url ?? '/', 'http://entry');
        const query = url.searchParams;
        if (method === 'GET' && path === '/healthz') {
          sendJson(res, 200, { ok: true });
          return;
        }
        const body = method === 'GET' || method === 'HEAD' ? Buffer.alloc(0) : await readBody(req);
        if (path === '/api/graphql' && method === 'POST') {
          await handleGraphql(req, res, body);
          return;
        }
        if (path === '/mcp') {
          await handleMcp(req, res, body);
          return;
        }
        if (path.startsWith('/api/v3')) {
          await handleRest(req, res, path, query, body);
          return;
        }
        if (await handleGit(req, res, path, query, body)) return;
        sendErr(res, 404, 'Not Found');
      } catch (err) {
        if (!res.headersSent) {
          try {
            sendErr(res, 500, err instanceof Error ? err.message : 'Internal Server Error');
          } catch {
            // peer gone
          }
        }
      }
    })();
  });
}
