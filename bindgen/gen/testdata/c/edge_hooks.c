/* The interceptor and the shims of edge.idl. */
#include <string.h>

#include "edge_server.h"

/* edge_guard: mode < 0 is denied, 100 answers an application status, 200 a reserved one, a name "evil" is
 * replaced by "good", and a mode above 50 is clamped. */
hqqrpc_status edge_intercept_edge_guard(hqqrpc_session *s, edge_args_edge_guard *a) {
    if (a->mode < 0)
        return HQQRPC_E_DENIED;
    if (a->mode == 100)
        return 0x0123u;
    if (a->mode == 200)
        return 9u;
    if (a->mode > 50)
        a->mode = 50;
    if (strcmp(a->name, "evil") == 0) {
        char *g = (char *)hqqrpc_scratch(s, 5);

        if (g == NULL)
            return HQQRPC_E_INTERNAL;
        memcpy(g, "good", 5);
        a->name = g;
        a->n = 4;
    }
    return HQQRPC_OK;
}

/* edge_cells: n cells alternating Int(i) and Text("ab"), and the failures of 4.6.6: -1 an application status,
 * -2 a reserved status, 4 one cell more than the bound (the shim reports the writer failure), 5 the same
 * but the shim ignores it and returns OK, 60 more bytes than max_out. */
hqqrpc_status edge_cells(int32_t n, hqqrpc_wr *ret) {
    size_t mark;
    int32_t i;
    hqqrpc_status st;

    if (n == -1)
        return 0x0123u;
    if (n == -2)
        return 7u;
    st = hqqrpc_wr_array_begin(ret, &mark);
    for (i = 0; st == HQQRPC_OK && i < n; i++)
        st = i % 2 == 0 ? edge_Cell_put_Int(ret, i) : edge_Cell_put_Text(ret, "ab", 2);
    if (n == 5)
        (void)hqqrpc_wr_array_end(ret, mark, (uint32_t)n, 3u);
    if (n == 5)
        return HQQRPC_OK;
    return st != HQQRPC_OK ? st : hqqrpc_wr_array_end(ret, mark, (uint32_t)n, 3u);
}

/* edge_pt_shim: a fixed struct whose padding is dirty; id -1 answers an application status. */
hqqrpc_status edge_pt_shim(int32_t id, edge_pt *ret) {
    memset(ret, 0xAA, sizeof *ret);
    if (id == -1)
        return 0x0100u;
    ret->x = id;
    ret->ok = id > 0;
    ret->y = (int64_t)id * 1000;
    return HQQRPC_OK;
}

/* edge_cell: one cell through every case writer; sel 14 and 15 write more than the IDL bounds allow. */
hqqrpc_status edge_cell(int32_t sel, hqqrpc_wr *ret) {
    edge_pt pts[2];
    const int32_t arr[3] = {1, 2, 3};

    memset(pts, 0xAA, sizeof pts);
    pts[0].x = 1;
    pts[0].ok = true;
    pts[0].y = 2;
    pts[1].x = 3;
    pts[1].ok = false;
    pts[1].y = 4;
    switch (sel) {
    case 1: return edge_Cell_put_Int(ret, -9);
    case 2: return edge_Cell_put_Text(ret, "abc", 3);
    case 3: return edge_Cell_put_Nul(ret);
    case 4: return edge_Cell_put_At(ret, &pts[0]);
    case 5: return edge_Cell_put_Arr(ret, arr, 3);
    case 6: return edge_Cell_put_Blob(ret, "xy", 2);
    case 7: return edge_Cell_put_Real(ret, 2.5);
    case 8: return edge_Cell_put_Byte(ret, 9);
    case 9: return edge_Cell_put_Flag(ret, true);
    case 10: return edge_Cell_put_Words(ret, pts, 2);
    case 11: return edge_Cell_put_Quad(ret, (const uint8_t *)"wxyz");
    case 12: return edge_Cell_put_Small(ret, -2);
    case 13: return edge_Cell_put_Wide(ret, 77);
    case 14: return edge_Cell_put_Text(ret, "abcde", 5);
    case 15: {
        edge_pt three[3];

        memset(three, 0, sizeof three);
        return edge_Cell_put_Words(ret, three, 3);
    }
    }
    return HQQRPC_OK;
}
