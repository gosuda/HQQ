/*
 * Syntax stand-in for GenMC's public header (lli/runtime-include/c/genmc.h,
 * GenMC v0.19.0). Used only by `run.sh --check`, which runs on hosts without
 * GenMC. GenMC builds include the installed header instead, never this file.
 * It declares only what the harnesses use: __VERIFIER_assume(cond), which in
 * GenMC is the macro __VERIFIER_assume_internal(cond, GENMC_ASSUME_USER) with
 * GENMC_ASSUME_USER == 0 (lli/runtime-include/c/genmc_internal.h).
 */
#ifndef HQQ_SYNTAX_GENMC_H
#define HQQ_SYNTAX_GENMC_H

#include <stdbool.h>

extern void __VERIFIER_assume_internal(bool cond, char type);
#define __VERIFIER_assume(cond) __VERIFIER_assume_internal((cond), 0)

#endif /* HQQ_SYNTAX_GENMC_H */
