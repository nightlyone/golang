// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

#include "runtime.h"
#include "defs_GOOS_GOARCH.h"
#include "signals_GOOS.h"
#include "os_GOOS.h"

extern void runtime·sigtramp(void);

typedef struct sigaction {
	union {
		void    (*__sa_handler)(int32);
		void    (*__sa_sigaction)(int32, Siginfo*, void *);
	} __sigaction_u;		/* signal handler */
	int32	sa_flags;		/* see signal options below */
	Sigset	sa_mask;		/* signal mask to apply */
} Sigaction;

void
runtime·dumpregs(Mcontext *r)
{
	runtime·printf("eax     %x\n", r->mc_eax);
	runtime·printf("ebx     %x\n", r->mc_ebx);
	runtime·printf("ecx     %x\n", r->mc_ecx);
	runtime·printf("edx     %x\n", r->mc_edx);
	runtime·printf("edi     %x\n", r->mc_edi);
	runtime·printf("esi     %x\n", r->mc_esi);
	runtime·printf("ebp     %x\n", r->mc_ebp);
	runtime·printf("esp     %x\n", r->mc_esp);
	runtime·printf("eip     %x\n", r->mc_eip);
	runtime·printf("eflags  %x\n", r->mc_eflags);
	runtime·printf("cs      %x\n", r->mc_cs);
	runtime·printf("fs      %x\n", r->mc_fs);
	runtime·printf("gs      %x\n", r->mc_gs);
}

void
runtime·sighandler(int32 sig, Siginfo *info, void *context, G *gp)
{
	Ucontext *uc;
	Mcontext *r;
	uintptr *sp;
	SigTab *t;

	uc = context;
	r = &uc->uc_mcontext;

	if(sig == SIGPROF) {
		runtime·sigprof((uint8*)r->mc_eip, (uint8*)r->mc_esp, nil, gp);
		return;
	}

	t = &runtime·sigtab[sig];
	if(info->si_code != SI_USER && (t->flags & SigPanic)) {
		if(gp == nil || gp == m->g0)
			goto Throw;
		// Make it look like a call to the signal func.
		// Have to pass arguments out of band since
		// augmenting the stack frame would break
		// the unwinding code.
		gp->sig = sig;
		gp->sigcode0 = info->si_code;
		gp->sigcode1 = (uintptr)info->si_addr;
		gp->sigpc = r->mc_eip;

		// Only push runtime·sigpanic if r->mc_eip != 0.
		// If r->mc_eip == 0, probably panicked because of a
		// call to a nil func.  Not pushing that onto sp will
		// make the trace look like a call to runtime·sigpanic instead.
		// (Otherwise the trace will end at runtime·sigpanic and we
		// won't get to see who faulted.)
		if(r->mc_eip != 0) {
			sp = (uintptr*)r->mc_esp;
			*--sp = r->mc_eip;
			r->mc_esp = (uintptr)sp;
		}
		r->mc_eip = (uintptr)runtime·sigpanic;
		return;
	}

	if(info->si_code == SI_USER || (t->flags & SigNotify))
		if(runtime·sigsend(sig))
			return;
	if(t->flags & SigKill)
		runtime·exit(2);
	if(!(t->flags & SigThrow))
		return;

Throw:
	runtime·startpanic();

	if(sig < 0 || sig >= NSIG)
		runtime·printf("Signal %d\n", sig);
	else
		runtime·printf("%s\n", runtime·sigtab[sig].name);

	runtime·printf("PC=%X\n", r->mc_eip);
	if(m->lockedg != nil && m->ncgo > 0 && gp == m->g0) {
		runtime·printf("signal arrived during cgo execution\n");
		gp = m->lockedg;
	}
	runtime·printf("\n");

	if(runtime·gotraceback()){
		runtime·traceback((void*)r->mc_eip, (void*)r->mc_esp, 0, gp);
		runtime·tracebackothers(gp);
		runtime·dumpregs(r);
	}

	runtime·exit(2);
}

void
runtime·signalstack(byte *p, int32 n)
{
	Sigaltstack st;

	st.ss_sp = (int8*)p;
	st.ss_size = n;
	st.ss_flags = 0;
	if(p == nil)
		st.ss_flags = SS_DISABLE;
	runtime·sigaltstack(&st, nil);
}

void
runtime·setsig(int32 i, void (*fn)(int32, Siginfo*, void*, G*), bool restart)
{
	Sigaction sa;

	// If SIGHUP handler is SIG_IGN, assume running
	// under nohup and do not set explicit handler.
	if(i == SIGHUP) {
		runtime·memclr((byte*)&sa, sizeof sa);
		runtime·sigaction(i, nil, &sa);
		if(sa.__sigaction_u.__sa_sigaction == SIG_IGN)
			return;
	}

	runtime·memclr((byte*)&sa, sizeof sa);
	sa.sa_flags = SA_SIGINFO|SA_ONSTACK;
	if(restart)
		sa.sa_flags |= SA_RESTART;
	sa.sa_mask.__bits[0] = ~(uint32)0;
	sa.sa_mask.__bits[1] = ~(uint32)0;
	sa.sa_mask.__bits[2] = ~(uint32)0;
	sa.sa_mask.__bits[3] = ~(uint32)0;
	if (fn == runtime·sighandler)
		fn = (void*)runtime·sigtramp;
	sa.__sigaction_u.__sa_sigaction = (void*)fn;
	runtime·sigaction(i, &sa, nil);
}
