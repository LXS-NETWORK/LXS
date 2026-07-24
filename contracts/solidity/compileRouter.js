const solc=require('solc'),fs=require('fs');
const input={language:'Solidity',sources:{'LxsRouter.sol':{content:fs.readFileSync('LxsRouter.sol','utf8')}},
 settings:{evmVersion:'istanbul',optimizer:{enabled:true,runs:200},outputSelection:{'*':{'*':['evm.bytecode.object','evm.methodIdentifiers']}}}};
const out=JSON.parse(solc.compile(JSON.stringify(input)));
let bad=false; if(out.errors) for(const e of out.errors){if(e.severity==='error'){console.error(e.formattedMessage);bad=true;}}
if(bad)process.exit(1);
const c=out.contracts['LxsRouter.sol']['LxsSwapRouter'];
fs.writeFileSync('LxsSwapRouter.bin',c.evm.bytecode.object); console.log('LxsSwapRouter len='+(c.evm.bytecode.object.length/2));
console.log('sels',JSON.stringify(c.evm.methodIdentifiers));
