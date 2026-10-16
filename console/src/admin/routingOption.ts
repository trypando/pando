// A routing adapter an app can move to, as GET /apps/{id}/routing lists it.

export interface RoutingOption {
  adapter_ref: string;
  name: string;
  modes: string[];
  default_mode: string;
  base_domain?: string;
  /** Every domain a hostname can be in, such as each Cloudflare zone. */
  domains?: string[];
  is_default: boolean;
}

/** What a hostname under this adapter has to end in, said as the helper. */
export function endingIn(option: RoutingOption | undefined): string | undefined {
  let domains = option?.domains ?? [];
  if (domains.length === 0 && option?.base_domain) domains = [option.base_domain];
  if (domains.length === 0) return undefined;
  if (domains.length === 1) return `A name ending in ${domains[0]}.`;
  return `A name ending in ${domains.slice(0, -1).join(', ')} or ${domains.at(-1)}.`;
}
