import removeEmail from "@/lib/api/client/app/emails/removeEmail";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import patchEmailLists from "./patchEmailLists";

export default function useRemoveEmail(id: string) {
    const queryClient = useQueryClient();

    return useMutation({
        mutationFn: () => removeEmail(id),
        onSuccess: () => {
            // Drop the row first so the list reacts immediately, then refetch.
            patchEmailLists(queryClient, (rows) => rows.filter((c) => c.id !== id));

            // The whole ["emails"] prefix, not just this mailbox: the allowance
            // counter lives under ["emails", "allowance"] and a disconnect gives
            // a slot back, so invalidating only ["emails", id] left the header
            // still claiming the workspace was at its limit.
            queryClient.invalidateQueries({ queryKey: ["emails"] });
            // Row health and the warmup coverage notice read from here.
            queryClient.invalidateQueries({ queryKey: ["analytics", "accounts"] });
        },
    });
}
