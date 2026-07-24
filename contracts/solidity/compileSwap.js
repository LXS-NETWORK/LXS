const solc=require('solc'),fs=require('fs');
const input={language:'Solidity',sources:{'LxsSwap.sol':{content:fs.readFileSync('LxsSwap.sol','utf8')}},
 settings:{evmVersion:'istanbul',optimizer:{enabled:true,runs:200},outputSelection:{'*':{'*':['evm.bytecode.object','evm.methodIdentifiers']}}}};
const out=JSON.parse(solc.compile(JSON.stringify(input)));
let bad=false; if(out.errors) for(const e of out.errors){if(e.severity==='error'){console.error(e.formattedMessage);bad=true;}}
if(bad)process.exit(1);
const cs=out.contracts['LxsSwap.sol'];
for(const n of ['WLXS','LxsSwapFactory','LxsSwapPair']){ fs.writeFileSync(n+'.bin',cs[n].evm.bytecode.object); console.log(n,'len='+(cs[n].evm.bytecode.object.length/2)); }
for(const n of ['WLXS','LxsSwapFactory','LxsSwapPair']) console.log(n,'sels',JSON.stringify(cs[n].evm.methodIdentifiers));
