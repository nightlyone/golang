// Inferno utils/8l/asm.c
// http://code.google.com/p/inferno-os/source/browse/utils/8l/asm.c
//
//	Copyright © 1994-1999 Lucent Technologies Inc.  All rights reserved.
//	Portions Copyright © 1995-1997 C H Forsyth (forsyth@terzarima.net)
//	Portions Copyright © 1997-1999 Vita Nuova Limited
//	Portions Copyright © 2000-2007 Vita Nuova Holdings Limited (www.vitanuova.com)
//	Portions Copyright © 2004,2006 Bruce Ellis
//	Portions Copyright © 2005-2007 C H Forsyth (forsyth@terzarima.net)
//	Revisions Copyright © 2000-2007 Lucent Technologies Inc. and others
//	Portions Copyright © 2009 The Go Authors.  All rights reserved.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT.  IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

// Data layout and relocation.

#include	"l.h"
#include	"../ld/lib.h"
#include	"../ld/elf.h"
#include	"../ld/pe.h"
#include	"../../pkg/runtime/mgc0.h"

void	dynreloc(void);
static vlong addaddrplus4(Sym *s, Sym *t, int32 add);

/*
 * divide-and-conquer list-link
 * sort of Sym* structures.
 * Used for the data block.
 */
int
datcmp(Sym *s1, Sym *s2)
{
	if(s1->type != s2->type)
		return (int)s1->type - (int)s2->type;
	if(s1->size != s2->size) {
		if(s1->size < s2->size)
			return -1;
		return +1;
	}
	return strcmp(s1->name, s2->name);
}

Sym*
datsort(Sym *l)
{
	Sym *l1, *l2, *le;

	if(l == 0 || l->next == 0)
		return l;

	l1 = l;
	l2 = l;
	for(;;) {
		l2 = l2->next;
		if(l2 == 0)
			break;
		l2 = l2->next;
		if(l2 == 0)
			break;
		l1 = l1->next;
	}

	l2 = l1->next;
	l1->next = 0;
	l1 = datsort(l);
	l2 = datsort(l2);

	/* set up lead element */
	if(datcmp(l1, l2) < 0) {
		l = l1;
		l1 = l1->next;
	} else {
		l = l2;
		l2 = l2->next;
	}
	le = l;

	for(;;) {
		if(l1 == 0) {
			while(l2) {
				le->next = l2;
				le = l2;
				l2 = l2->next;
			}
			le->next = 0;
			break;
		}
		if(l2 == 0) {
			while(l1) {
				le->next = l1;
				le = l1;
				l1 = l1->next;
			}
			break;
		}
		if(datcmp(l1, l2) < 0) {
			le->next = l1;
			le = l1;
			l1 = l1->next;
		} else {
			le->next = l2;
			le = l2;
			l2 = l2->next;
		}
	}
	le->next = 0;
	return l;
}

Reloc*
addrel(Sym *s)
{
	if(s->nr >= s->maxr) {
		if(s->maxr == 0)
			s->maxr = 4;
		else
			s->maxr <<= 1;
		s->r = realloc(s->r, s->maxr*sizeof s->r[0]);
		if(s->r == 0) {
			diag("out of memory");
			errorexit();
		}
		memset(s->r+s->nr, 0, (s->maxr-s->nr)*sizeof s->r[0]);
	}
	return &s->r[s->nr++];
}

void
relocsym(Sym *s)
{
	Reloc *r;
	Sym *rs;
	Prog p;
	int32 i, off, siz, fl;
	vlong o;
	uchar *cast;

	cursym = s;
	memset(&p, 0, sizeof p);
	for(r=s->r; r<s->r+s->nr; r++) {
		off = r->off;
		siz = r->siz;
		if(off < 0 || off+siz > s->np) {
			diag("%s: invalid relocation %d+%d not in [%d,%d)", s->name, off, siz, 0, s->np);
			continue;
		}
		if(r->sym != S && (r->sym->type & SMASK == 0 || r->sym->type & SMASK == SXREF)) {
			diag("%s: not defined", r->sym->name);
			continue;
		}
		if(r->type >= 256)
			continue;

		if(r->sym != S && r->sym->type == SDYNIMPORT)
			diag("unhandled relocation for %s (type %d rtype %d)", r->sym->name, r->sym->type, r->type);

		if(r->sym != S && !r->sym->reachable)
			diag("unreachable sym in relocation: %s %s", s->name, r->sym->name);

		switch(r->type) {
		default:
			o = 0;
			if(isobj || archreloc(r, s, &o) < 0)
				diag("unknown reloc %d", r->type);
			break;
		case D_ADDR:
			o = symaddr(r->sym) + r->add;
			if(isobj && r->sym->type != SCONST) {
				if(thechar == '6')
					o = 0;
				else {
					// set up addend for eventual relocation via outer symbol
					rs = r->sym;
					while(rs->outer != nil)
						rs = rs->outer;
					o -= symaddr(rs);
				}
			}
			break;
		case D_PCREL:
			// r->sym can be null when CALL $(constant) is transformed from absolute PC to relative PC call.
			o = 0;
			if(r->sym)
				o += symaddr(r->sym);
			o += r->add - (s->value + r->off + r->siz);
			if(isobj && r->sym->type != SCONST) {
				if(thechar == '6')
					o = 0;
				else
					o = r->add - r->siz;
			}
			break;
		case D_SIZE:
			o = r->sym->size + r->add;
			break;
		}
//print("relocate %s %p %s => %p %p %p %p [%p]\n", s->name, s->value+off, r->sym ? r->sym->name : "<nil>", (void*)symaddr(r->sym), (void*)s->value, (void*)r->off, (void*)r->siz, (void*)o);
		switch(siz) {
		default:
			cursym = s;
			diag("bad reloc size %#ux for %s", siz, r->sym->name);
		case 4:
			fl = o;
			cast = (uchar*)&fl;
			for(i=0; i<4; i++)
				s->p[off+i] = cast[inuxi4[i]];
			break;
		case 8:
			cast = (uchar*)&o;
			for(i=0; i<8; i++)
				s->p[off+i] = cast[inuxi8[i]];
			break;
		}
	}
}

void
reloc(void)
{
	Sym *s;

	if(debug['v'])
		Bprint(&bso, "%5.2f reloc\n", cputime());
	Bflush(&bso);

	for(s=textp; s!=S; s=s->next)
		relocsym(s);
	for(s=datap; s!=S; s=s->next)
		relocsym(s);
}

void
dynrelocsym(Sym *s)
{
	Reloc *r;
	Sym *rel;
	Sym *got;
	
	if(HEADTYPE == Hwindows) {
		Sym *rel, *targ;

		rel = lookup(".rel", 0);
		if(s == rel)
			return;
		for(r=s->r; r<s->r+s->nr; r++) {
			targ = r->sym;
			if(r->sym->plt == -2 && r->sym->got != -2) { // make dynimport JMP table for PE object files.
				targ->plt = rel->size;
				r->sym = rel;
				r->add = targ->plt;

				// jmp *addr
				if(thechar == '8') {
					adduint8(rel, 0xff);
					adduint8(rel, 0x25);
					addaddr(rel, targ);
					adduint8(rel, 0x90);
					adduint8(rel, 0x90);
				} else {
					adduint8(rel, 0xff);
					adduint8(rel, 0x24);
					adduint8(rel, 0x25);
					addaddrplus4(rel, targ, 0);
					adduint8(rel, 0x90);
				}
			} else if(r->sym->plt >= 0) {
				r->sym = rel;
				r->add = targ->plt;
			}
		}
		return;
	}

	got = rel = nil;
	if(flag_shared) {
		rel = lookuprel();
		got = lookup(".got", 0);
	}
	s->rel_ro = 0;
	for(r=s->r; r<s->r+s->nr; r++) {
		if(r->sym != S && r->sym->type == SDYNIMPORT || r->type >= 256)
			adddynrel(s, r);
		if(flag_shared && r->sym != S && (r->sym->dynimpname == nil || r->sym->dynexport) && r->type == D_ADDR
				&& (s == got || s->type == SDATA || s->type == SGOSTRING || s->type == STYPE || s->type == SRODATA)) {
			// Create address based RELATIVE relocation
			adddynrela(rel, s, r);
			if(s->type < SNOPTRDATA)
				s->rel_ro = 1;
		}
	}
}

void
dynreloc(void)
{
	Sym *s;

	// -d supresses dynamic loader format, so we may as well not
	// compute these sections or mark their symbols as reachable.
	if(debug['d'] && HEADTYPE != Hwindows)
		return;
	if(debug['v'])
		Bprint(&bso, "%5.2f reloc\n", cputime());
	Bflush(&bso);

	for(s=textp; s!=S; s=s->next)
		dynrelocsym(s);
	for(s=datap; s!=S; s=s->next)
		dynrelocsym(s);
	if(iself)
		elfdynhash();
}

void
symgrow(Sym *s, int32 siz)
{
	if(s->np >= siz)
		return;

	if(s->maxp < siz) {
		if(s->maxp == 0)
			s->maxp = 8;
		while(s->maxp < siz)
			s->maxp <<= 1;
		s->p = realloc(s->p, s->maxp);
		if(s->p == nil) {
			diag("out of memory");
			errorexit();
		}
		memset(s->p+s->np, 0, s->maxp-s->np);
	}
	s->np = siz;
}

void
savedata(Sym *s, Prog *p, char *pn)
{
	int32 off, siz, i, fl;
	uchar *cast;
	vlong o;
	Reloc *r;

	off = p->from.offset;
	siz = p->datasize;
	if(off < 0 || siz < 0 || off >= 1<<30 || siz >= 100)
		mangle(pn);
	symgrow(s, off+siz);

	switch(p->to.type) {
	default:
		diag("bad data: %P", p);
		break;

	case D_FCONST:
		switch(siz) {
		default:
		case 4:
			fl = ieeedtof(&p->to.ieee);
			cast = (uchar*)&fl;
			for(i=0; i<4; i++)
				s->p[off+i] = cast[fnuxi4[i]];
			break;
		case 8:
			cast = (uchar*)&p->to.ieee;
			for(i=0; i<8; i++)
				s->p[off+i] = cast[fnuxi8[i]];
			break;
		}
		break;

	case D_SCONST:
		for(i=0; i<siz; i++)
			s->p[off+i] = p->to.scon[i];
		break;

	case D_CONST:
		if(p->to.sym)
			goto Addr;
		o = p->to.offset;
		fl = o;
		cast = (uchar*)&fl;
		switch(siz) {
		default:
			diag("bad nuxi %d\n%P", siz, p);
			break;
		case 1:
			s->p[off] = cast[inuxi1[0]];
			break;
		case 2:
			for(i=0; i<2; i++)
				s->p[off+i] = cast[inuxi2[i]];
			break;
		case 4:
			for(i=0; i<4; i++)
				s->p[off+i] = cast[inuxi4[i]];
			break;
		case 8:
			cast = (uchar*)&o;
			for(i=0; i<8; i++)
				s->p[off+i] = cast[inuxi8[i]];
			break;
		}
		break;

	case D_ADDR:
	case D_SIZE:
	Addr:
		r = addrel(s);
		r->off = off;
		r->siz = siz;
		r->sym = p->to.sym;
		r->type = p->to.type;
		if(r->type != D_SIZE)
			r->type = D_ADDR;
		r->add = p->to.offset;
		break;
	}
}

static void
blk(Sym *start, int32 addr, int32 size)
{
	Sym *sym;
	int32 eaddr;
	uchar *p, *ep;

	for(sym = start; sym != nil; sym = sym->next)
		if(!(sym->type&SSUB) && sym->value >= addr)
			break;

	eaddr = addr+size;
	for(; sym != nil; sym = sym->next) {
		if(sym->type&SSUB)
			continue;
		if(sym->value >= eaddr)
			break;
		if(sym->value < addr) {
			diag("phase error: addr=%#llx but sym=%#llx type=%d", (vlong)addr, (vlong)sym->value, sym->type);
			errorexit();
		}
		cursym = sym;
		for(; addr < sym->value; addr++)
			cput(0);
		p = sym->p;
		ep = p + sym->np;
		while(p < ep)
			cput(*p++);
		addr += sym->np;
		for(; addr < sym->value+sym->size; addr++)
			cput(0);
		if(addr != sym->value+sym->size) {
			diag("phase error: addr=%#llx value+size=%#llx", (vlong)addr, (vlong)sym->value+sym->size);
			errorexit();
		}
	}

	for(; addr < eaddr; addr++)
		cput(0);
	cflush();
}

void
codeblk(int32 addr, int32 size)
{
	Sym *sym;
	int32 eaddr, n, epc;
	Prog *p;
	uchar *q;

	if(debug['a'])
		Bprint(&bso, "codeblk [%#x,%#x) at offset %#llx\n", addr, addr+size, cpos());

	blk(textp, addr, size);

	/* again for printing */
	if(!debug['a'])
		return;

	for(sym = textp; sym != nil; sym = sym->next) {
		if(!sym->reachable)
			continue;
		if(sym->value >= addr)
			break;
	}

	eaddr = addr + size;
	for(; sym != nil; sym = sym->next) {
		if(!sym->reachable)
			continue;
		if(sym->value >= eaddr)
			break;

		if(addr < sym->value) {
			Bprint(&bso, "%-20s %.8llux|", "_", (vlong)addr);
			for(; addr < sym->value; addr++)
				Bprint(&bso, " %.2ux", 0);
			Bprint(&bso, "\n");
		}
		p = sym->text;
		if(p == nil) {
			Bprint(&bso, "%.6llux\t%-20s | foreign text\n", (vlong)addr, sym->name);
			n = sym->size;
			q = sym->p;

			while(n >= 16) {
				Bprint(&bso, "%.6ux\t%-20.16I\n", addr, q);
				addr += 16;
				q += 16;
				n -= 16;
			}
			if(n > 0)
				Bprint(&bso, "%.6ux\t%-20.*I\n", addr, (int)n, q);
			addr += n;
			continue;
		}

		Bprint(&bso, "%.6llux\t%-20s | %P\n", (vlong)sym->value, sym->name, p);
		for(p = p->link; p != P; p = p->link) {
			if(p->link != P)
				epc = p->link->pc;
			else
				epc = sym->value + sym->size;
			Bprint(&bso, "%.6llux\t", (uvlong)p->pc);
			q = sym->p + p->pc - sym->value;
			n = epc - p->pc;
			Bprint(&bso, "%-20.*I | %P\n", (int)n, q, p);
			addr += n;
		}
	}

	if(addr < eaddr) {
		Bprint(&bso, "%-20s %.8llux|", "_", (vlong)addr);
		for(; addr < eaddr; addr++)
			Bprint(&bso, " %.2ux", 0);
	}
	Bflush(&bso);
}

void
datblk(int32 addr, int32 size)
{
	Sym *sym;
	int32 eaddr;
	uchar *p, *ep;

	if(debug['a'])
		Bprint(&bso, "datblk [%#x,%#x) at offset %#llx\n", addr, addr+size, cpos());

	blk(datap, addr, size);

	/* again for printing */
	if(!debug['a'])
		return;

	for(sym = datap; sym != nil; sym = sym->next)
		if(sym->value >= addr)
			break;

	eaddr = addr + size;
	for(; sym != nil; sym = sym->next) {
		if(sym->value >= eaddr)
			break;
		if(addr < sym->value) {
			Bprint(&bso, "%-20s %.8ux| 00 ...\n", "(pre-pad)", addr);
			addr = sym->value;
		}
		Bprint(&bso, "%-20s %.8ux|", sym->name, (uint)addr);
		p = sym->p;
		ep = p + sym->np;
		while(p < ep)
			Bprint(&bso, " %.2ux", *p++);
		addr += sym->np;
		for(; addr < sym->value+sym->size; addr++)
			Bprint(&bso, " %.2ux", 0);
		Bprint(&bso, "\n");
	}

	if(addr < eaddr)
		Bprint(&bso, "%-20s %.8ux| 00 ...\n", "(post-pad)", (uint)addr);
	Bprint(&bso, "%-20s %.8ux|\n", "", (uint)eaddr);
}

void
strnput(char *s, int n)
{
	for(; n > 0 && *s; s++) {
		cput(*s);
		n--;
	}
	while(n > 0) {
		cput(0);
		n--;
	}
}

void
addstrdata(char *name, char *value)
{
	Sym *s, *sp;
	char *p;

	p = smprint("%s.str", name);
	sp = lookup(p, 0);
	free(p);
	addstring(sp, value);

	s = lookup(name, 0);
	s->size = 0;
	s->dupok = 1;
	addaddr(s, sp);
	adduint32(s, strlen(value));
	if(PtrSize == 8)
		adduint32(s, 0);  // round struct to pointer width

	// in case reachability has already been computed
	sp->reachable = s->reachable;
}

vlong
addstring(Sym *s, char *str)
{
	int n;
	int32 r;

	if(s->type == 0)
		s->type = SNOPTRDATA;
	s->reachable = 1;
	r = s->size;
	n = strlen(str)+1;
	if(strcmp(s->name, ".shstrtab") == 0)
		elfsetstring(str, r);
	symgrow(s, r+n);
	memmove(s->p+r, str, n);
	s->size += n;
	return r;
}

vlong
setuintxx(Sym *s, vlong off, uint64 v, int wid)
{
	int32 i, fl;
	vlong o;
	uchar *cast;

	if(s->type == 0)
		s->type = SDATA;
	s->reachable = 1;
	if(s->size < off+wid) {
		s->size = off+wid;
		symgrow(s, s->size);
	}
	fl = v;
	cast = (uchar*)&fl;
	switch(wid) {
	case 1:
		s->p[off] = cast[inuxi1[0]];
		break;
	case 2:
		for(i=0; i<2; i++)
			s->p[off+i] = cast[inuxi2[i]];
		break;
	case 4:
		for(i=0; i<4; i++)
			s->p[off+i] = cast[inuxi4[i]];
		break;
	case 8:
		o = v;
		cast = (uchar*)&o;
		for(i=0; i<8; i++)
			s->p[off+i] = cast[inuxi8[i]];
		break;
	}
	return off;
}

vlong
adduintxx(Sym *s, uint64 v, int wid)
{
	int32 off;

	off = s->size;
	setuintxx(s, off, v, wid);
	return off;
}

vlong
adduint8(Sym *s, uint8 v)
{
	return adduintxx(s, v, 1);
}

vlong
adduint16(Sym *s, uint16 v)
{
	return adduintxx(s, v, 2);
}

vlong
adduint32(Sym *s, uint32 v)
{
	return adduintxx(s, v, 4);
}

vlong
adduint64(Sym *s, uint64 v)
{
	return adduintxx(s, v, 8);
}

void
setuint8(Sym *s, vlong r, uint8 v)
{
	setuintxx(s, r, v, 1);
}

void
setuint16(Sym *s, vlong r, uint16 v)
{
	setuintxx(s, r, v, 2);
}

void
setuint32(Sym *s, vlong r, uint32 v)
{
	setuintxx(s, r, v, 4);
}

void
setuint64(Sym *s, vlong r, uint64 v)
{
	setuintxx(s, r, v, 8);
}

vlong
addaddrplus(Sym *s, Sym *t, int32 add)
{
	vlong i;
	Reloc *r;

	if(s->type == 0)
		s->type = SDATA;
	s->reachable = 1;
	i = s->size;
	s->size += PtrSize;
	symgrow(s, s->size);
	r = addrel(s);
	r->sym = t;
	r->off = i;
	r->siz = PtrSize;
	r->type = D_ADDR;
	r->add = add;
	return i;
}

static vlong
addaddrplus4(Sym *s, Sym *t, int32 add)
{
	vlong i;
	Reloc *r;

	if(s->type == 0)
		s->type = SDATA;
	s->reachable = 1;
	i = s->size;
	s->size += 4;
	symgrow(s, s->size);
	r = addrel(s);
	r->sym = t;
	r->off = i;
	r->siz = 4;
	r->type = D_ADDR;
	r->add = add;
	return i;
}

vlong
addpcrelplus(Sym *s, Sym *t, int32 add)
{
	vlong i;
	Reloc *r;

	if(s->type == 0)
		s->type = SDATA;
	s->reachable = 1;
	i = s->size;
	s->size += 4;
	symgrow(s, s->size);
	r = addrel(s);
	r->sym = t;
	r->off = i;
	r->add = add;
	r->type = D_PCREL;
	r->siz = 4;
	return i;
}

vlong
addaddr(Sym *s, Sym *t)
{
	return addaddrplus(s, t, 0);
}

vlong
setaddrplus(Sym *s, vlong off, Sym *t, int32 add)
{
	Reloc *r;

	if(s->type == 0)
		s->type = SDATA;
	s->reachable = 1;
	if(off+PtrSize > s->size) {
		s->size = off + PtrSize;
		symgrow(s, s->size);
	}
	r = addrel(s);
	r->sym = t;
	r->off = off;
	r->siz = PtrSize;
	r->type = D_ADDR;
	r->add = add;
	return off;
}

vlong
setaddr(Sym *s, vlong off, Sym *t)
{
	return setaddrplus(s, off, t, 0);
}

vlong
addsize(Sym *s, Sym *t)
{
	vlong i;
	Reloc *r;

	if(s->type == 0)
		s->type = SDATA;
	s->reachable = 1;
	i = s->size;
	s->size += PtrSize;
	symgrow(s, s->size);
	r = addrel(s);
	r->sym = t;
	r->off = i;
	r->siz = PtrSize;
	r->type = D_SIZE;
	return i;
}

void
dosymtype(void)
{
	Sym *s;

	for(s = allsym; s != nil; s = s->allsym) {
		if(s->np > 0) {
			if(s->type == SBSS)
				s->type = SDATA;
			if(s->type == SNOPTRBSS)
				s->type = SNOPTRDATA;
		}
	}
}

static int32
alignsymsize(int32 s)
{
	if(s >= 8)
		s = rnd(s, 8);
	else if(s >= PtrSize)
		s = rnd(s, PtrSize);
	else if(s > 2)
		s = rnd(s, 4);
	return s;
}

static int32
aligndatsize(int32 datsize, Sym *s)
{
	int32 t;

	if(s->align != 0) {
		datsize = rnd(datsize, s->align);
	} else {
		t = alignsymsize(s->size);
		if(t & 1) {
			;
		} else if(t & 2)
			datsize = rnd(datsize, 2);
		else if(t & 4)
			datsize = rnd(datsize, 4);
		else
			datsize = rnd(datsize, 8);
	}
	return datsize;
}

static void
gcaddsym(Sym *gc, Sym *s, int32 off)
{
	int32 a;
	Sym *gotype;

	if(s->size < PtrSize)
		return;
	if(strcmp(s->name, ".string") == 0)
		return;

	gotype = s->gotype;
	if(gotype != nil) {
		//print("gcaddsym:    %s    %d    %s\n", s->name, s->size, gotype->name);
		adduintxx(gc, GC_CALL, PtrSize);
		adduintxx(gc, off, PtrSize);
		addpcrelplus(gc, decodetype_gc(gotype), 3*PtrSize+4);
		if(PtrSize == 8)
			adduintxx(gc, 0, 4);
	} else {
		//print("gcaddsym:    %s    %d    <unknown type>\n", s->name, s->size);
		for(a = -off&(PtrSize-1); a+PtrSize<=s->size; a+=PtrSize) {
			adduintxx(gc, GC_APTR, PtrSize);
			adduintxx(gc, off+a, PtrSize);
		}
	}
}

void
dodata(void)
{
	int32 t, datsize;
	Section *sect;
	Sym *s, *last, **l;
	Sym *gcdata1, *gcbss1;

	if(debug['v'])
		Bprint(&bso, "%5.2f dodata\n", cputime());
	Bflush(&bso);

	// define garbage collection symbols
	gcdata1 = lookup("gcdata1", 0);
	gcdata1->type = SGCDATA;
	gcdata1->reachable = 1;
	gcbss1 = lookup("gcbss1", 0);
	gcbss1->type = SGCBSS;
	gcbss1->reachable = 1;

	// size of .data and .bss section. the zero value is later replaced by the actual size of the section.
	adduintxx(gcdata1, 0, PtrSize);
	adduintxx(gcbss1, 0, PtrSize);

	last = nil;
	datap = nil;

	for(s=allsym; s!=S; s=s->allsym) {
		if(!s->reachable || s->special)
			continue;
		if(STEXT < s->type && s->type < SXREF) {
			if(last == nil)
				datap = s;
			else
				last->next = s;
			s->next = nil;
			last = s;
		}
	}

	for(s = datap; s != nil; s = s->next) {
		if(s->np > s->size)
			diag("%s: initialize bounds (%lld < %d)",
				s->name, (vlong)s->size, s->np);
	}


	/*
	 * now that we have the datap list, but before we start
	 * to assign addresses, record all the necessary
	 * dynamic relocations.  these will grow the relocation
	 * symbol, which is itself data.
	 */
	dynreloc();

	/* some symbols may no longer belong in datap (Mach-O) */
	for(l=&datap; (s=*l) != nil; ) {
		if(s->type <= STEXT || SXREF <= s->type)
			*l = s->next;
		else
			l = &s->next;
	}
	*l = nil;

	if(flag_shared) {
		for(s=datap; s != nil; s = s->next) {
			if(s->rel_ro)
				s->type = SDATARELRO;
		}
	}
	datap = datsort(datap);

	/*
	 * allocate sections.  list is sorted by type,
	 * so we can just walk it for each piece we want to emit.
	 * segdata is processed before segtext, because we need
	 * to see all symbols in the .data and .bss sections in order
	 * to generate garbage collection information.
	 */

	/* begin segdata */

	/* skip symbols belonging to segtext */
	s = datap;
	for(; s != nil && s->type < SELFSECT; s = s->next)
		;

	/* writable ELF sections */
	datsize = 0;
	for(; s != nil && s->type < SNOPTRDATA; s = s->next) {
		sect = addsection(&segdata, s->name, 06);
		if(s->align != 0)
			datsize = rnd(datsize, s->align);
		sect->vaddr = datsize;
		s->sect = sect;
		s->type = SDATA;
		s->value = datsize;
		datsize += rnd(s->size, PtrSize);
		sect->len = datsize - sect->vaddr;
	}

	/* pointer-free data */
	sect = addsection(&segdata, ".noptrdata", 06);
	sect->vaddr = datsize;
	lookup("noptrdata", 0)->sect = sect;
	lookup("enoptrdata", 0)->sect = sect;
	for(; s != nil && s->type < SDATARELRO; s = s->next) {
		s->sect = sect;
		s->type = SDATA;
		t = alignsymsize(s->size);
		datsize = aligndatsize(datsize, s);
		s->value = datsize;
		datsize += t;
	}
	sect->len = datsize - sect->vaddr;
	datsize = rnd(datsize, PtrSize);

	/* dynamic relocated rodata */
	if(flag_shared) {
		sect = addsection(&segdata, ".data.rel.ro", 06);
		sect->vaddr = datsize;
		lookup("datarelro", 0)->sect = sect;
		lookup("edatarelro", 0)->sect = sect;
		for(; s != nil && s->type == SDATARELRO; s = s->next) {
			if(s->align != 0)
				datsize = rnd(datsize, s->align);
			s->sect = sect;
			s->type = SDATA;
			s->value = datsize;
			datsize += rnd(s->size, PtrSize);
		}
		sect->len = datsize - sect->vaddr;
		datsize = rnd(datsize, PtrSize);
	}

	/* data */
	sect = addsection(&segdata, ".data", 06);
	sect->vaddr = datsize;
	lookup("data", 0)->sect = sect;
	lookup("edata", 0)->sect = sect;
	for(; s != nil && s->type < SBSS; s = s->next) {
		if(s->type == SDATARELRO) {
			cursym = s;
			diag("unexpected symbol type %d", s->type);
		}
		s->sect = sect;
		s->type = SDATA;
		t = alignsymsize(s->size);
		datsize = aligndatsize(datsize, s);
		s->value = datsize;
		gcaddsym(gcdata1, s, datsize - sect->vaddr);  // gc
		datsize += t;
	}
	sect->len = datsize - sect->vaddr;
	datsize = rnd(datsize, PtrSize);

	adduintxx(gcdata1, GC_END, PtrSize);
	setuintxx(gcdata1, 0, sect->len, PtrSize);

	/* bss */
	sect = addsection(&segdata, ".bss", 06);
	sect->vaddr = datsize;
	lookup("bss", 0)->sect = sect;
	lookup("ebss", 0)->sect = sect;
	for(; s != nil && s->type < SNOPTRBSS; s = s->next) {
		s->sect = sect;
		t = alignsymsize(s->size);
		datsize = aligndatsize(datsize, s);
		s->value = datsize;
		gcaddsym(gcbss1, s, datsize - sect->vaddr);  // gc
		datsize += t;
	}
	sect->len = datsize - sect->vaddr;
	datsize = rnd(datsize, PtrSize);

	adduintxx(gcbss1, GC_END, PtrSize);
	setuintxx(gcbss1, 0, sect->len, PtrSize);

	/* pointer-free bss */
	sect = addsection(&segdata, ".noptrbss", 06);
	sect->vaddr = datsize;
	lookup("noptrbss", 0)->sect = sect;
	lookup("enoptrbss", 0)->sect = sect;
	for(; s != nil; s = s->next) {
		if(s->type > SNOPTRBSS) {
			cursym = s;
			diag("unexpected symbol type %d", s->type);
		}
		s->sect = sect;
		t = alignsymsize(s->size);
		datsize = aligndatsize(datsize, s);
		s->value = datsize;
		datsize += t;
	}
	sect->len = datsize - sect->vaddr;
	lookup("end", 0)->sect = sect;

	/* we finished segdata, begin segtext */

	/* read-only data */
	sect = addsection(&segtext, ".rodata", 04);
	sect->vaddr = 0;
	lookup("rodata", 0)->sect = sect;
	lookup("erodata", 0)->sect = sect;
	datsize = 0;
	s = datap;
	for(; s != nil && s->type < STYPELINK; s = s->next) {
		s->sect = sect;
		if(s->align != 0)
			datsize = rnd(datsize, s->align);
		s->type = SRODATA;
		s->value = datsize;
		datsize += rnd(s->size, PtrSize);
	}
	sect->len = datsize - sect->vaddr;
	datsize = rnd(datsize, PtrSize);

	/* type */
	sect = addsection(&segtext, ".typelink", 04);
	sect->vaddr = datsize;
	lookup("typelink", 0)->sect = sect;
	lookup("etypelink", 0)->sect = sect;
	for(; s != nil && s->type == STYPELINK; s = s->next) {
		s->sect = sect;
		s->type = SRODATA;
		s->value = datsize;
		datsize += s->size;
	}
	sect->len = datsize - sect->vaddr;
	datsize = rnd(datsize, PtrSize);

	/* gcdata */
	sect = addsection(&segtext, ".gcdata", 04);
	sect->vaddr = datsize;
	lookup("gcdata", 0)->sect = sect;
	lookup("egcdata", 0)->sect = sect;
	for(; s != nil && s->type == SGCDATA; s = s->next) {
		s->sect = sect;
		s->type = SRODATA;
		s->value = datsize;
		datsize += s->size;
	}
	sect->len = datsize - sect->vaddr;
	datsize = rnd(datsize, PtrSize);

	/* gcbss */
	sect = addsection(&segtext, ".gcbss", 04);
	sect->vaddr = datsize;
	lookup("gcbss", 0)->sect = sect;
	lookup("egcbss", 0)->sect = sect;
	for(; s != nil && s->type == SGCBSS; s = s->next) {
		s->sect = sect;
		s->type = SRODATA;
		s->value = datsize;
		datsize += s->size;
	}
	sect->len = datsize - sect->vaddr;
	datsize = rnd(datsize, PtrSize);

	/* gosymtab */
	sect = addsection(&segtext, ".gosymtab", 04);
	sect->vaddr = datsize;
	lookup("symtab", 0)->sect = sect;
	lookup("esymtab", 0)->sect = sect;
	for(; s != nil && s->type < SPCLNTAB; s = s->next) {
		s->sect = sect;
		s->type = SRODATA;
		s->value = datsize;
		datsize += s->size;
	}
	sect->len = datsize - sect->vaddr;
	datsize = rnd(datsize, PtrSize);

	/* gopclntab */
	sect = addsection(&segtext, ".gopclntab", 04);
	sect->vaddr = datsize;
	lookup("pclntab", 0)->sect = sect;
	lookup("epclntab", 0)->sect = sect;
	for(; s != nil && s->type < SELFROSECT; s = s->next) {
		s->sect = sect;
		s->type = SRODATA;
		s->value = datsize;
		datsize += s->size;
	}
	sect->len = datsize - sect->vaddr;
	datsize = rnd(datsize, PtrSize);

	/* read-only ELF sections */
	for(; s != nil && s->type < SELFSECT; s = s->next) {
		sect = addsection(&segtext, s->name, 04);
		if(s->align != 0)
			datsize = rnd(datsize, s->align);
		sect->vaddr = datsize;
		s->sect = sect;
		s->type = SRODATA;
		s->value = datsize;
		datsize += rnd(s->size, PtrSize);
		sect->len = datsize - sect->vaddr;
	}
}

// assign addresses to text
void
textaddress(void)
{
	uvlong va;
	Prog *p;
	Section *sect;
	Sym *sym, *sub;

	addsection(&segtext, ".text", 05);

	// Assign PCs in text segment.
	// Could parallelize, by assigning to text
	// and then letting threads copy down, but probably not worth it.
	sect = segtext.sect;
	lookup("text", 0)->sect = sect;
	lookup("etext", 0)->sect = sect;
	va = INITTEXT;
	sect->vaddr = va;
	for(sym = textp; sym != nil; sym = sym->next) {
		sym->sect = sect;
		if(sym->type & SSUB)
			continue;
		if(sym->align != 0)
			va = rnd(va, sym->align);
		else if(sym->text != P)
			va = rnd(va, FuncAlign);
		sym->value = 0;
		for(sub = sym; sub != S; sub = sub->sub) {
			sub->value += va;
			for(p = sub->text; p != P; p = p->link)
				p->pc += sub->value;
		}
		if(sym->size == 0 && sym->sub != S) {
			cursym = sym;
		}
		va += sym->size;
	}

	// Align end of code so that rodata starts aligned.
	// 128 bytes is likely overkill but definitely cheap.
	va = rnd(va, 128);

	sect->len = va - sect->vaddr;
}

// assign addresses
void
address(void)
{
	Section *s, *text, *data, *rodata, *symtab, *pclntab, *noptr, *bss, *noptrbss, *datarelro;
	Section *gcdata, *gcbss, *typelink;
	Sym *sym, *sub;
	uvlong va;

	va = INITTEXT;
	segtext.rwx = 05;
	segtext.vaddr = va;
	segtext.fileoff = HEADR;
	for(s=segtext.sect; s != nil; s=s->next) {
		s->vaddr = va;
		va += rnd(s->len, PtrSize);
	}
	segtext.len = va - INITTEXT;
	segtext.filelen = segtext.len;

	va = rnd(va, INITRND);

	segdata.rwx = 06;
	segdata.vaddr = va;
	segdata.fileoff = va - segtext.vaddr + segtext.fileoff;
	segdata.filelen = 0;
	if(HEADTYPE == Hwindows)
		segdata.fileoff = segtext.fileoff + rnd(segtext.len, PEFILEALIGN);
	if(HEADTYPE == Hplan9x64 || HEADTYPE == Hplan9x32)
		segdata.fileoff = segtext.fileoff + segtext.filelen;
	data = nil;
	noptr = nil;
	bss = nil;
	noptrbss = nil;
	datarelro = nil;
	for(s=segdata.sect; s != nil; s=s->next) {
		s->vaddr = va;
		va += s->len;
		segdata.filelen += s->len;
		segdata.len = va - segdata.vaddr;
		if(strcmp(s->name, ".data") == 0)
			data = s;
		if(strcmp(s->name, ".noptrdata") == 0)
			noptr = s;
		if(strcmp(s->name, ".bss") == 0)
			bss = s;
		if(strcmp(s->name, ".noptrbss") == 0)
			noptrbss = s;
		if(strcmp(s->name, ".data.rel.ro") == 0)
			datarelro = s;
	}
	segdata.filelen -= bss->len + noptrbss->len; // deduct .bss

	text = segtext.sect;
	rodata = text->next;
	typelink = rodata->next;
	gcdata = typelink->next;
	gcbss = gcdata->next;
	symtab = gcbss->next;
	pclntab = symtab->next;

	for(sym = datap; sym != nil; sym = sym->next) {
		cursym = sym;
		if(sym->type < SNOPTRDATA)
			sym->value += rodata->vaddr;
		else
			sym->value += segdata.sect->vaddr;
		for(sub = sym->sub; sub != nil; sub = sub->sub)
			sub->value += sym->value;
	}

	xdefine("text", STEXT, text->vaddr);
	xdefine("etext", STEXT, text->vaddr + text->len);
	xdefine("rodata", SRODATA, rodata->vaddr);
	xdefine("erodata", SRODATA, rodata->vaddr + rodata->len);
	xdefine("typelink", SRODATA, typelink->vaddr);
	xdefine("etypelink", SRODATA, typelink->vaddr + typelink->len);
	if(datarelro != nil) {
		xdefine("datarelro", SRODATA, datarelro->vaddr);
		xdefine("edatarelro", SRODATA, datarelro->vaddr + datarelro->len);
	}
	xdefine("gcdata", SGCDATA, gcdata->vaddr);
	xdefine("egcdata", SGCDATA, gcdata->vaddr + gcdata->len);
	xdefine("gcbss", SGCBSS, gcbss->vaddr);
	xdefine("egcbss", SGCBSS, gcbss->vaddr + gcbss->len);
	xdefine("symtab", SRODATA, symtab->vaddr);
	xdefine("esymtab", SRODATA, symtab->vaddr + symtab->len);
	xdefine("pclntab", SRODATA, pclntab->vaddr);
	xdefine("epclntab", SRODATA, pclntab->vaddr + pclntab->len);
	xdefine("noptrdata", SNOPTRDATA, noptr->vaddr);
	xdefine("enoptrdata", SNOPTRDATA, noptr->vaddr + noptr->len);
	xdefine("bss", SBSS, bss->vaddr);
	xdefine("ebss", SBSS, bss->vaddr + bss->len);
	xdefine("data", SDATA, data->vaddr);
	xdefine("edata", SDATA, data->vaddr + data->len);
	xdefine("noptrbss", SNOPTRBSS, noptrbss->vaddr);
	xdefine("enoptrbss", SNOPTRBSS, noptrbss->vaddr + noptrbss->len);
	xdefine("end", SBSS, segdata.vaddr + segdata.len);
}
