import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "react-router";

import { endpoints } from "../lib/api";
import { useCustomers } from "@/lib/useCustomers";
import { toMinorUnits, formatCurrency, shortId, BILLING_CURRENCIES } from "@/lib/utils";
import { FormField } from "@/components/patterns/FormField";
import { Button } from "@/components/ui/button";
import { Textarea } from "@/components/ui/textarea";
import { Input } from "@/components/ui/input";
import {
  Sheet,
  SheetContent,
  SheetHeader,
  SheetTitle,
  SheetDescription,
  SheetFooter,
} from "@/components/ui/sheet";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";

// Credit note types the API accepts (POST /v1/credit-notes `type`).
const ADJUSTMENT = "adjustment";
const REFUND = "refund";

const CreateCreditNote = () => {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  // Shared cached customer list (limit 1000 — the old raw fetch defaulted to
  // limit=10 and silently truncated the dropdown).
  const { customers } = useCustomers();
  const [error, setError] = useState(null);
  const [errors, setErrors] = useState({});
  const [formData, setFormData] = useState({
    customer_id: "",
    type: ADJUSTMENT,
    amount: "",
    // Empty until known: the customer's billing currency, the linked
    // invoice's, or the operator's pick — never an assumed USD.
    currency: "",
    reason: "",
    invoice_id: "", // Optional
    expires_at: "", // Optional — blank means the credit never expires
  });

  // Invoices for the invoice picker. Fetched once and filtered client-side to
  // the selected customer — a credit note may only link one of that customer's
  // own invoices, never a UUID typed by hand (which risked cross-customer links
  // and mismatched currencies).
  const { data: allInvoices = [] } = useQuery({
    queryKey: ["invoices", "for-credit-note"],
    queryFn: async () => (await endpoints.getInvoices({ per_page: 250 })).data?.data || [],
  });
  const customerInvoices = useMemo(
    () =>
      formData.customer_id
        ? allInvoices.filter((i) => i.customer_id === formData.customer_id)
        : [],
    [allInvoices, formData.customer_id]
  );
  const linkedInvoice = customerInvoices.find((i) => i.id === formData.invoice_id);
  // The API refunds only a paid invoice, up to what was paid on it (less any
  // earlier refunds, which the server enforces).
  const canRefund = linkedInvoice?.status === "paid" && (linkedInvoice.amount_paid || 0) > 0;
  const isRefund = formData.type === REFUND;

  const close = () => navigate("/credit-notes");

  // Selecting a customer resets any linked invoice (the list changes) and
  // defaults the currency to the one that customer is billed in (their latest
  // invoice), keeping the current pick when they have no invoices yet.
  const selectCustomer = (id) =>
    setFormData((prev) => {
      const billed = allInvoices.find((i) => i.customer_id === id)?.currency;
      return {
        ...prev,
        customer_id: id,
        invoice_id: "",
        type: ADJUSTMENT,
        currency: billed || prev.currency,
      };
    });

  // Linking an invoice locks the credit to that invoice's currency — a credit
  // against an invoice must be issued in the same currency. A refund needs a
  // paid invoice, so changing the link drops back to a balance credit.
  const NONE = "__none__";
  const selectInvoice = (value) => {
    if (value === NONE) {
      setFormData((prev) => ({ ...prev, invoice_id: "", type: ADJUSTMENT }));
      return;
    }
    const inv = customerInvoices.find((i) => i.id === value);
    setFormData((prev) => ({
      ...prev,
      invoice_id: value,
      type: ADJUSTMENT,
      currency: inv?.currency || prev.currency,
    }));
  };

  const handleChange = (e) => {
    setFormData({ ...formData, [e.target.name]: e.target.value });
  };

  const validate = () => {
    const next = {};
    if (!formData.customer_id) next.customer_id = "Select a customer.";
    if (!formData.currency) next.currency = "Select a currency.";
    if (!formData.amount) next.amount = "Enter a credit amount.";
    else if (
      isRefund &&
      toMinorUnits(formData.amount, formData.currency) > (linkedInvoice?.amount_paid || 0)
    )
      next.amount = "A refund can't be more than was paid on the invoice.";
    setErrors(next);
    return Object.keys(next).length === 0;
  };

  const createMutation = useMutation({
    mutationFn: (payload) => endpoints.createCreditNote(payload),
    onSuccess: (res, payload) => {
      // Read-your-write: the list is cached for 60s, so invalidate before
      // navigating or the new credit note won't show until the cache expires.
      queryClient.invalidateQueries({ queryKey: ["credit-notes"] });
      // A refund's outcome (refund_status: succeeded, failed, manual) shows on
      // the note's own page, so open it rather than the list.
      const id = res?.data?.data?.id;
      navigate(payload.type === REFUND && id ? `/credit-notes/${id}` : "/credit-notes");
    },
    onError: (err) => {
      console.error(err);
      setError(
        err?.response?.data?.error?.message || "Failed to create credit note"
      );
    },
  });
  const loading = createMutation.isPending;

  const handleSubmit = (e) => {
    e.preventDefault();
    if (!validate()) return;
    setError(null);

    // Convert amount to cents
    const payload = {
      ...formData,
      amount: toMinorUnits(formData.amount, formData.currency),
      invoice_id: formData.invoice_id ? formData.invoice_id : null,
    };
    if (!payload.invoice_id) delete payload.invoice_id;
    // Expiry is optional and applies only to a spendable balance credit; send
    // an end-of-day RFC3339 timestamp so the credit is usable through the
    // selected date, or omit it entirely (never expires).
    if (formData.expires_at && !isRefund) {
      payload.expires_at = new Date(`${formData.expires_at}T23:59:59Z`).toISOString();
    } else {
      delete payload.expires_at;
    }

    createMutation.mutate(payload);
  };

  const typeDescription = isRefund
    ? `Sends the money back through the payment gateway to the card or account that paid. Up to ${formatCurrency(linkedInvoice?.amount_paid || 0, formData.currency)}, less any earlier refunds. A payment taken offline is marked for a manual refund.`
    : canRefund
      ? "Adds spendable credit the customer's next invoices draw down. No money moves."
      : "Adds spendable credit the customer's next invoices draw down. A refund needs a linked paid invoice.";

  return (
    <Sheet open onOpenChange={(open) => !open && close()}>
      <SheetContent side="right" className="w-full sm:max-w-lg">
        <SheetHeader>
          <SheetTitle>Create credit note</SheetTitle>
          <SheetDescription>
            Credit a customer's balance, or refund a paid invoice to the original payment.
          </SheetDescription>
        </SheetHeader>

        <form
          id="create-credit-note-form"
          onSubmit={handleSubmit}
          className="flex-1 space-y-6 overflow-y-auto px-6 py-6"
        >
          {error && (
            <div className="rounded-lg bg-destructive/5 px-4 py-3 text-sm text-destructive ring-1 ring-inset ring-destructive/20">
              {error}
            </div>
          )}

          <FormField
            label="Customer"
            htmlFor="customer_id"
            required
            error={errors.customer_id}
          >
            <Select
              value={formData.customer_id}
              onValueChange={selectCustomer}
            >
              <SelectTrigger id="customer_id">
                <SelectValue placeholder="Select a customer..." />
              </SelectTrigger>
              <SelectContent>
                {customers.map((c) => (
                  <SelectItem key={c.id} value={c.id}>
                    {c.name} ({c.email})
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </FormField>

          <FormField
            label="Linked invoice (optional)"
            htmlFor="invoice_id"
            description={
              formData.invoice_id
                ? "The credit is issued in this invoice's currency."
                : undefined
            }
          >
            <Select
              value={
                formData.customer_id ? formData.invoice_id || NONE : undefined
              }
              onValueChange={selectInvoice}
              disabled={!formData.customer_id}
            >
              <SelectTrigger id="invoice_id">
                <SelectValue
                  placeholder={
                    formData.customer_id
                      ? "No linked invoice"
                      : "Select a customer first"
                  }
                />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value={NONE}>No linked invoice</SelectItem>
                {customerInvoices.map((inv) => (
                  <SelectItem key={inv.id} value={inv.id}>
                    {(inv.invoice_number || shortId(inv.id))} —{" "}
                    {formatCurrency(inv.total, inv.currency)} · {inv.status}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </FormField>

          <FormField label="Type" htmlFor="type" description={typeDescription}>
            <Select
              value={formData.type}
              onValueChange={(v) => setFormData((prev) => ({ ...prev, type: v }))}
            >
              <SelectTrigger id="type">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value={ADJUSTMENT}>Credit to balance</SelectItem>
                <SelectItem value={REFUND} disabled={!canRefund}>
                  Refund to the original payment
                </SelectItem>
              </SelectContent>
            </Select>
          </FormField>

          <div className="grid grid-cols-1 gap-6 sm:grid-cols-2">
            <FormField
              label={isRefund ? "Refund amount" : "Credit amount"}
              htmlFor="amount"
              required
              error={errors.amount}
            >
              <div className="relative">
                <span className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-sm text-muted-foreground">
                  {formData.currency}
                </span>
                <Input
                  id="amount"
                  type="number"
                  step="0.01"
                  name="amount"
                  value={formData.amount}
                  onChange={handleChange}
                  placeholder="0.00"
                  className="pl-12"
                />
              </div>
            </FormField>

            {!formData.invoice_id && (
              <FormField
                label="Currency"
                htmlFor="currency"
                required
                error={errors.currency}
              >
                <Select
                  value={formData.currency || undefined}
                  onValueChange={(v) => setFormData((prev) => ({ ...prev, currency: v }))}
                >
                  <SelectTrigger id="currency">
                    <SelectValue placeholder="Select currency" />
                  </SelectTrigger>
                  <SelectContent>
                    {BILLING_CURRENCIES.map((c) => (
                      <SelectItem key={c} value={c}>
                        {c}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </FormField>
            )}
          </div>

          {!isRefund && (
            <FormField
              label="Expires on (optional)"
              htmlFor="expires_at"
              description="Leave blank for a credit that never expires. On this date, any unused balance is written off."
            >
              <Input
                id="expires_at"
                type="date"
                name="expires_at"
                value={formData.expires_at}
                onChange={handleChange}
                className="sm:max-w-[12rem]"
              />
            </FormField>
          )}

          <FormField label={isRefund ? "Reason for refund" : "Reason for credit"} htmlFor="reason">
            <Textarea
              id="reason"
              name="reason"
              rows={4}
              value={formData.reason}
              onChange={handleChange}
              placeholder="e.g. Service downtime compensation"
            />
          </FormField>
        </form>

        <SheetFooter>
          <Button type="button" variant="outline" onClick={close}>
            Cancel
          </Button>
          <Button type="submit" form="create-credit-note-form" disabled={loading}>
            {loading
              ? "Issuing..."
              : isRefund
                ? "Issue refund"
                : "Issue credit note"}
          </Button>
        </SheetFooter>
      </SheetContent>
    </Sheet>
  );
};

export default CreateCreditNote;
