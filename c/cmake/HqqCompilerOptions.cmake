# Compiler flags, atomics requirements and sanitizer selection for libhqq.
#
# Provides:
#   HQQ_LIB_TYPE                   STATIC or SHARED
#   HQQ_C11_ATOMICS_FLAGS_STR      flags needed to compile <stdatomic.h> code
#   HQQ_C11_ATOMICS_FORCE_DEFINE   ON when __STDC_NO_ATOMICS__ must be overridden
#   hqq_apply_compile_options(<target>)

if(HQQ_BUILD_SHARED)
  set(HQQ_LIB_TYPE SHARED)
else()
  set(HQQ_LIB_TYPE STATIC)
endif()

set(HQQ_C11_ATOMICS_FLAGS "")
set(HQQ_C11_ATOMICS_FORCE_DEFINE OFF)
set(HQQ_IS_MSVC_CL OFF)    # cl.exe itself
set(HQQ_IS_CLANG_CL OFF)   # clang-cl (MSVC command line, clang front end)

if(MSVC)
  if(CMAKE_C_COMPILER_ID STREQUAL "Clang")
    set(HQQ_IS_CLANG_CL ON)
  else()
    set(HQQ_IS_MSVC_CL ON)
    # cl.exe: C11 atomics are experimental (VS 2022 17.5+) and need both
    # /std:c11 (set by CMAKE_C_STANDARD) and /experimental:c11atomics. The
    # compiler keeps defining __STDC_NO_ATOMICS__, which hqq.h bypasses.
    list(APPEND HQQ_C11_ATOMICS_FLAGS /std:c11 /experimental:c11atomics)
    set(HQQ_C11_ATOMICS_FORCE_DEFINE ON)
  endif()
endif()
string(JOIN " " HQQ_C11_ATOMICS_FLAGS_STR ${HQQ_C11_ATOMICS_FLAGS})

# ---- Sanitizers -------------------------------------------------------------
set(HQQ_SANITIZER_FLAGS "")
if(NOT HQQ_SANITIZE STREQUAL "none")
  if(MSVC AND NOT HQQ_IS_CLANG_CL)
    if(HQQ_SANITIZE STREQUAL "address")
      list(APPEND HQQ_SANITIZER_FLAGS /fsanitize=address)
    else()
      message(FATAL_ERROR "cl.exe supports only HQQ_SANITIZE=address")
    endif()
  else()
    if(HQQ_SANITIZE STREQUAL "address")
      set(_san address)
    elseif(HQQ_SANITIZE STREQUAL "undefined")
      set(_san undefined)
    elseif(HQQ_SANITIZE STREQUAL "address+undefined")
      set(_san address,undefined)
    elseif(HQQ_SANITIZE STREQUAL "thread")
      set(_san thread)
    else()
      message(FATAL_ERROR "Unknown HQQ_SANITIZE value '${HQQ_SANITIZE}'")
    endif()
    list(APPEND HQQ_SANITIZER_FLAGS -fsanitize=${_san} -fno-omit-frame-pointer)
    if(HQQ_SANITIZE MATCHES "undefined")
      # Make UBSan findings fatal so that ctest fails on them.
      list(APPEND HQQ_SANITIZER_FLAGS -fno-sanitize-recover=undefined)
    endif()
    # Sanitizer runtimes need a stable, debuggable code shape.
    list(APPEND HQQ_SANITIZER_FLAGS -g -O1)
  endif()
endif()

function(hqq_apply_compile_options target)
  if(NOT WIN32)
    # Feature-test macros for every POSIX target, once: glibc under -std=c11
    # hides POSIX and BSD names (O_CLOEXEC, ftruncate, nanosleep, MAP_ANON, ...);
    # Darwin ignores _DEFAULT_SOURCE, so macOS is unchanged.
    target_compile_definitions(${target} PRIVATE _DEFAULT_SOURCE)
  endif()
  if(MSVC)
    # The tools and tests use portable CRT names (fopen, fscanf); MSVC-family
    # compilers flag them as deprecated (C4996 / -Wdeprecated-declarations).
    target_compile_definitions(${target} PRIVATE _CRT_SECURE_NO_WARNINGS _CRT_NONSTDC_NO_WARNINGS)
    target_compile_options(${target} PRIVATE /W4)
    if(HQQ_WERROR)
      target_compile_options(${target} PRIVATE /WX)
    endif()
    if(HQQ_IS_MSVC_CL)
      target_compile_options(${target} PRIVATE
        $<$<COMPILE_LANGUAGE:C>:/experimental:c11atomics>
        # C4125: 3-digit octal escape followed by a digit in generated literals.
        # C4127 (constant conditional) fires on test macros that check
        # compile-time constants; C4706 fires on `if ((st = fn()) != 0)`.
        /wd4125 /wd4127 /wd4706)
    endif()
  else()
    target_compile_options(${target} PRIVATE -Wall -Wextra -Wpedantic)
    if(HQQ_WERROR)
      target_compile_options(${target} PRIVATE -Werror)
    endif()
  endif()
  if(HQQ_SANITIZER_FLAGS)
    target_compile_options(${target} PRIVATE ${HQQ_SANITIZER_FLAGS})
    # PUBLIC: anything linking libhqq must link the sanitizer runtime too.
    target_link_options(${target} PUBLIC ${HQQ_SANITIZER_FLAGS})
  endif()
endfunction()
