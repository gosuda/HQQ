// process_id.hpp - the process ID for unique segment names, portable across POSIX
// and Windows (MSVC and clang-cl). Used by the tests and examples only; the wrapper
// header hqq/hqq.hpp does not depend on it.
#pragma once

#if defined(_WIN32)
#include <process.h>  // _getpid
#else
#include <unistd.h>  // getpid
#endif

namespace hqq_support {

inline long process_id() {
#if defined(_WIN32)
    return static_cast<long>(_getpid());
#else
    return static_cast<long>(::getpid());
#endif
}

}  // namespace hqq_support
