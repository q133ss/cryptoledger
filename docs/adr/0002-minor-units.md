# Integer amounts and scale

The API carries signed 64-bit minor units. Each account fixes its currency and scale. A posting of 1000000 at scale 6 represents one unit. Equal currency and scale are required for every account in the transaction. Both backends persist integer amounts. The service checks addition for overflow and limits posting magnitude. Cross-currency conversion requires a separately defined exchange operation.
