# Command-line tools (interop and manual checks). Built with the tests.
add_executable(hqq_echo hqq_echo.c)
target_link_libraries(hqq_echo PRIVATE hqq::hqq)
hqq_apply_compile_options(hqq_echo)
